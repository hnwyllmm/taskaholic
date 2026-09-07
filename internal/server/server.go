package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"work-assistant/internal/backup"

	"github.com/coder/websocket"

	"work-assistant/internal/concierge"
	"work-assistant/internal/gitlabci"
	"work-assistant/internal/model"
	"work-assistant/internal/rolebuilder"
	"work-assistant/internal/router"
	"work-assistant/internal/rpcpeer"
	"work-assistant/internal/store"
	"work-assistant/internal/taskaction"
	"work-assistant/internal/tasksource"
	"work-assistant/internal/workflow"
)

type Config struct {
	TestPipelineExecutor     gitlabci.Executor
	TestPipelineReader       gitlabci.Reader
	LocalRuntimeID           string
	HomeAssistant            concierge.Assistant
	WorkContract             workflow.Contract
	ReviewContract           workflow.Contract
	AgentSelector            router.AgentSelector
	ReviewPlanner            router.ReviewPlanner
	RoleBuilder              rolebuilder.Builder
	Listen                   string
	RuntimeToken             string
	APIToken                 string
	TLSCertFile              string
	TLSKeyFile               string
	UpgradeEnabled           bool
	UpgradeValidationSandbox string
	InstanceID               string
	Backups                  *backup.Manager
}

type Server struct {
	homeAssistant concierge.Assistant
	workContract  workflow.Contract
	config        Config
	store         *store.Store
	log           *slog.Logger
	hub           *runtimeHub
	router        router.Selector
	agentRouter   router.AgentSelector
	reviewPlanner router.ReviewPlanner
	roleBuilder   rolebuilder.Builder
	http          *http.Server
	sources       *tasksource.Engine
	testActions   *taskaction.PipelineActions
}

func New(config Config, state *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{config: config, store: state, log: logger, hub: newRuntimeHub(), router: router.FirstOnline{}}
	server.sources = tasksource.New(state)
	executor := config.TestPipelineExecutor
	if executor == nil {
		executor = gitlabci.New()
	}
	server.testActions = taskaction.NewPipelines(state, executor)
	if config.TestPipelineReader != nil {
		server.sources.Providers["gitlab"] = &tasksource.GitLab{Read: config.TestPipelineReader, Lookup: state.GetTestPipeline}
	}
	server.homeAssistant = config.HomeAssistant
	if server.homeAssistant == nil {
		server.homeAssistant = concierge.JSONAssistant{}
	}
	server.workContract = config.WorkContract
	if server.workContract == nil {
		server.workContract = workflow.JSONContract{}
	}
	server.agentRouter, server.roleBuilder = config.AgentSelector, config.RoleBuilder
	if server.agentRouter == nil {
		server.agentRouter = router.LeastLoaded{}
	}
	server.reviewPlanner = config.ReviewPlanner
	if server.reviewPlanner == nil {
		server.reviewPlanner = router.CapabilityReviews{}
	}
	if server.roleBuilder == nil {
		server.roleBuilder = rolebuilder.JSONBuilder{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", server.handleRoot)
	mux.HandleFunc("GET /health/live", server.handleLive)
	mux.HandleFunc("GET /health/ready", server.handleReady)
	mux.HandleFunc("GET /api/v1/auth/config", server.handleAuthConfig)
	mux.Handle("POST /api/v1/tasks", server.apiAuth(http.HandlerFunc(server.handleCreateTask)))
	mux.Handle("GET /api/v1/tasks", server.apiAuth(http.HandlerFunc(server.handleListTasks)))
	mux.Handle("GET /api/v1/tasks/{task_id}", server.apiAuth(http.HandlerFunc(server.handleGetTask)))
	mux.Handle("POST /api/v1/tasks/{task_id}/subtasks", server.apiAuth(http.HandlerFunc(server.handleCreateSubtask)))
	mux.Handle("POST /api/v1/tasks/{task_id}/runs", server.apiAuth(http.HandlerFunc(server.handleCreateRun)))
	mux.Handle("GET /api/v1/runs/{run_id}", server.apiAuth(http.HandlerFunc(server.handleGetRun)))
	mux.Handle("POST /api/v1/runs/{run_id}/interrupt", server.apiAuth(http.HandlerFunc(server.handleInterruptRun)))
	mux.Handle("POST /api/v1/runs/{run_id}/directives", server.apiAuth(http.HandlerFunc(server.handleCreateDirective)))
	mux.Handle("GET /api/v1/directives/{directive_id}", server.apiAuth(http.HandlerFunc(server.handleGetDirective)))
	mux.Handle("GET /api/v1/sessions", server.apiAuth(http.HandlerFunc(server.handleListSessions)))
	mux.Handle("GET /api/v1/sessions/{session_id}", server.apiAuth(http.HandlerFunc(server.handleGetSession)))
	mux.Handle("GET /api/v1/runtimes", server.apiAuth(http.HandlerFunc(server.handleListRuntimes)))
	mux.Handle("GET /api/v1/runtimes/{runtime_id}/models", server.apiAuth(http.HandlerFunc(server.handleListModels)))
	mux.Handle("GET /api/v1/events/stream", server.apiAuth(http.HandlerFunc(server.handleEventStream)))
	mux.Handle("GET /api/v1/admin/backup", server.apiAuth(http.HandlerFunc(server.handleBackup)))
	mux.Handle("GET /api/v1/admin/backups", server.apiAuth(http.HandlerFunc(server.handleBackups)))
	mux.Handle("POST /api/v1/admin/backups", server.apiAuth(http.HandlerFunc(server.handleCreateBackup)))
	mux.HandleFunc("GET /runtime/ws", server.handleRuntimeWebSocket)
	server.registerRoleRoutes(mux)
	server.registerWorkRoutes(mux)
	server.registerSourceRoutes(mux)
	server.registerHomeRoutes(mux)
	server.registerUpgradeRoutes(mux)
	server.registerSystemAgentRoutes(mux)
	mux.HandleFunc("GET /tasks", server.handleWorkUI)
	mux.HandleFunc("GET /tasks/", server.handleWorkUI)
	mux.HandleFunc("GET /roles", server.handleRoleUI)
	mux.HandleFunc("GET /roles/", server.handleRoleUI)
	mux.HandleFunc("GET /members", server.handleRoleUI)
	mux.HandleFunc("GET /members/", server.handleRoleUI)
	server.http = &http.Server{
		Addr:              config.Listen,
		Handler:           requestLogger(logger, mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return server
}

func (s *Server) Run(ctx context.Context) error {
	// This MVP has one control-plane writer. A process restart invalidates every
	// persisted connection; runtimes become ONLINE again only after runtime.hello.
	if err := s.store.MarkAllRuntimesOffline(ctx); err != nil {
		return fmt.Errorf("reset stale runtime presence: %w", err)
	}
	dispatchCtx, cancelDispatch := context.WithCancel(ctx)
	defer cancelDispatch()
	if s.config.Backups != nil {
		done := make(chan struct{})
		go func() { defer close(done); s.config.Backups.Run(dispatchCtx) }()
		defer func() { cancelDispatch(); <-done }()
	}
	go s.dispatchLoop(dispatchCtx)
	go s.workLoop(dispatchCtx)
	sourceDone := make(chan struct{})
	go func() { defer close(sourceDone); s.sourceLoop(dispatchCtx) }()
	defer func() { cancelDispatch(); <-sourceDone }()
	actionDone := make(chan struct{})
	go func() { defer close(actionDone); serverActionLoop(dispatchCtx, s) }()
	defer func() { cancelDispatch(); <-actionDone }()

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("control server listening", "address", s.config.Listen, "tls", s.config.TLSCertFile != "")
		var err error
		if s.config.TLSCertFile != "" || s.config.TLSKeyFile != "" {
			if s.config.TLSCertFile == "" || s.config.TLSKeyFile == "" {
				err = errors.New("both tls-cert and tls-key are required")
			} else {
				err = s.http.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile)
			}
		} else {
			err = s.http.ListenAndServe()
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	s.handlePageUI(w, r)
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "live"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "instance_id": s.config.InstanceID})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Requirements   model.TaskRequirements `json:"requirements"`
		Title          string                 `json:"title"`
		Goal           string                 `json:"goal"`
		IdempotencyKey string                 `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	task, replayed, err := s.store.CreateTask(r.Context(), request.IdempotencyKey, request.Title, request.Goal, request.Requirements)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, status, task)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tasks, err := s.store.ListTasks(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if tasks == nil {
		tasks = []model.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	detail, err := s.store.GetTaskDetail(r.Context(), r.PathValue("task_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SessionID       string   `json:"session_id"`
		ReasoningEffort *string  `json:"reasoning_effort,omitempty"`
		RuntimeID       string   `json:"runtime_id"`
		AgentID         string   `json:"agent_id"`
		AdapterID       string   `json:"adapter_id"`
		ModelID         string   `json:"model_id"`
		Command         []string `json:"command"`
		WorkingDir      string   `json:"working_dir"`
		IdempotencyKey  string   `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	task, err := s.store.GetTask(r.Context(), r.PathValue("task_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resolved, err := s.resolveRun(r.Context(), task, store.CreateRunRequest{
		TaskID: task.ID, SessionID: request.SessionID, RuntimeID: request.RuntimeID,
		AgentID: request.AgentID, AdapterID: request.AdapterID, ModelID: request.ModelID,
		ReasoningEffort: request.ReasoningEffort,
		Command:         request.Command, WorkingDir: request.WorkingDir, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	run, err := s.store.CreateRun(r.Context(), resolved)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) handleCreateSubtask(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Requirements   model.TaskRequirements `json:"requirements"`
		Title          string                 `json:"title"`
		Goal           string                 `json:"goal"`
		CreatedByRunID string                 `json:"created_by_run_id"`
		IdempotencyKey string                 `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	result, replayed, err := s.store.CreateSubtask(r.Context(), r.PathValue("task_id"),
		request.CreatedByRunID, request.IdempotencyKey, request.Title, request.Goal, request.Requirements)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, status, result)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.Context(), r.PathValue("run_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleInterruptRun(w http.ResponseWriter, r *http.Request) {
	commandID, err := s.store.InterruptRun(r.Context(), r.PathValue("run_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"command_id": commandID, "status": "QUEUED"})
}

func (s *Server) handleCreateDirective(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Kind           string `json:"kind"`
		Message        string `json:"message"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	directive, err := s.store.CreateDirective(r.Context(), r.PathValue("run_id"), request.Kind, request.Message, request.IdempotencyKey)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, directive)
}

func (s *Server) handleGetDirective(w http.ResponseWriter, r *http.Request) {
	directive, err := s.store.GetDirective(r.Context(), r.PathValue("directive_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, directive)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	sessions, err := s.store.ListSessions(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if sessions == nil {
		sessions = []model.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	detail, err := s.store.GetSessionDetail(r.Context(), r.PathValue("session_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleListRuntimes(w http.ResponseWriter, r *http.Request) {
	runtimes, err := s.store.ListRuntimes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if runtimes == nil {
		runtimes = []model.Runtime{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtimes": runtimes})
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	temporary, err := os.CreateTemp("", "work-assistant-backup-*.sqlite")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	path := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// VACUUM INTO intentionally refuses to overwrite an existing file.
	if err := os.Remove(path); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer os.Remove(path)
	if err := s.store.Backup(r.Context(), path); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="work-assistant-backup.sqlite"`)
	http.ServeFile(w, r, path)
}

func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	if taskID != "" {
		if _, err := s.store.GetTask(r.Context(), taskID); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is unsupported"))
		return
	}
	after, err := nonnegativeCursor(r.URL.Query().Get("after"))
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if header := r.Header.Get("Last-Event-ID"); header != "" {
		parsed, e := nonnegativeCursor(header)
		if e != nil {
			writeError(w, 400, e)
			return
		}
		if parsed > after {
			after = parsed
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	if _, err = fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastHeartbeat := time.Now()
	for {
		events, err := s.store.EventsAfter(r.Context(), after, 100, taskID)
		if err != nil {
			s.log.Error("read SSE events", "error", err)
			return
		}
		for _, event := range events {
			encoded, _ := json.Marshal(event)
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.GlobalSeq, event.Type, encoded); err != nil {
				return
			}
			after = event.GlobalSeq
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if time.Since(lastHeartbeat) >= 5*time.Second {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
			lastHeartbeat = time.Now()
		}
		if len(events) == 100 {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) handleRuntimeWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.config.RuntimeToken != "" && !tokenMatches(r.Header.Get("Authorization"), s.config.RuntimeToken) {
		writeError(w, http.StatusUnauthorized, errors.New("invalid runtime token"))
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	ctx := r.Context()
	var runtimeID, epoch string
	var peer *rpcpeer.Peer
	peer = rpcpeer.New(conn, func(callCtx context.Context, request rpcpeer.Request) (any, *rpcpeer.Error) {
		switch request.Method {
		case "runtime.hello":
			var hello model.RuntimeHello
			if err := json.Unmarshal(request.Params, &hello); err != nil {
				return nil, invalidParams(err)
			}
			if hello.RuntimeID == "" || hello.Epoch == "" {
				return nil, invalidParams(errors.New("runtime_id and epoch are required"))
			}
			if runtimeID != "" {
				return nil, &rpcpeer.Error{Code: -32001, Message: "runtime already initialized"}
			}
			if err := s.store.RegisterRuntime(callCtx, hello); err != nil {
				return nil, internalRPCError(err)
			}
			runtimeID, epoch = hello.RuntimeID, hello.Epoch
			s.hub.register(&runtimeConnection{runtimeID: runtimeID, epoch: epoch, peer: peer})
			s.log.Info("runtime connected", "runtime_id", runtimeID, "epoch", epoch, "host", hello.Hostname)
			return map[string]any{"accepted": true, "server_time_ms": time.Now().UTC().UnixMilli()}, nil
		case "runtime.heartbeat":
			if runtimeID == "" {
				return nil, &rpcpeer.Error{Code: -32002, Message: "runtime.hello required"}
			}
			if err := s.store.HeartbeatRuntime(callCtx, runtimeID, epoch); err != nil {
				return nil, internalRPCError(err)
			}
			return map[string]any{"ack": true, "server_time_ms": time.Now().UTC().UnixMilli()}, nil
		case "run.event":
			if runtimeID == "" {
				return nil, &rpcpeer.Error{Code: -32002, Message: "runtime.hello required"}
			}
			var event model.RuntimeEvent
			if err := json.Unmarshal(request.Params, &event); err != nil {
				return nil, invalidParams(err)
			}
			if event.RuntimeID != runtimeID {
				return nil, &rpcpeer.Error{Code: -32003, Message: "runtime_id mismatch"}
			}
			duplicate, err := s.store.ApplyRuntimeEventFrom(callCtx, epoch, event)
			if err != nil {
				return nil, internalRPCError(err)
			}
			return map[string]any{"durable": true, "duplicate": duplicate, "runtime_seq": event.RuntimeSeq}, nil
		default:
			return nil, &rpcpeer.Error{Code: -32601, Message: "method not found"}
		}
	})
	err = peer.Serve(ctx)
	if runtimeID != "" {
		s.hub.unregister(runtimeID, peer)
		_ = s.store.DisconnectRuntime(context.Background(), runtimeID, epoch)
		s.log.Info("runtime disconnected", "runtime_id", runtimeID, "epoch", epoch, "error", err)
	}
}

func (s *Server) dispatchLoop(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, connection := range s.hub.snapshot() {
				s.dispatchOne(ctx, connection)
			}
		}
	}
}

func (s *Server) dispatchOne(ctx context.Context, connection *runtimeConnection) {
	message, err := s.store.ClaimOutbox(ctx, connection.runtimeID, "assistantd")
	if err != nil {
		s.log.Error("claim outbox", "runtime_id", connection.runtimeID, "error", err)
		return
	}
	if message == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response struct {
		Accepted bool `json:"accepted"`
	}
	if err := connection.peer.CallID(callCtx, message.ID, message.Method, message.Params, &response); err != nil {
		_ = s.store.RetryOutbox(context.Background(), message.ID, err.Error(), message.Attempts)
		s.log.Warn("dispatch failed", "message_id", message.ID, "runtime_id", connection.runtimeID, "error", err)
		return
	}
	if !response.Accepted {
		_ = s.store.RetryOutbox(context.Background(), message.ID, "runtime rejected message", message.Attempts)
		return
	}
	if err := s.store.MarkOutboxDelivered(context.Background(), message.ID); err != nil {
		s.log.Error("mark outbox delivered", "message_id", message.ID, "error", err)
	}
}

func (s *Server) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Loopback deployments without a token must still reject cross-origin
		// browser writes. CLI/API clients without Origin remain supported.
		if origin := r.Header.Get("Origin"); origin != "" && r.Method != "GET" && r.Method != "HEAD" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				writeError(w, http.StatusForbidden, errors.New("cross-origin writes are not allowed"))
				return
			}
		}
		if s.config.APIToken != "" && !tokenMatches(r.Header.Get("Authorization"), s.config.APIToken) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid API token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tokenMatches(header, expected string) bool {
	provided := strings.TrimPrefix(header, "Bearer ")
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("body must contain one JSON value")
	}
	return nil
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, model.ErrConflict) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if errors.Is(err, model.ErrValidation) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("resource not found"))
		return
	}
	message := err.Error()
	if strings.Contains(message, "conflicts") || strings.Contains(message, "already bound") ||
		strings.Contains(message, "is not active") || strings.Contains(message, "active run") ||
		strings.Contains(message, "cannot be decomposed") {
		writeError(w, http.StatusConflict, err)
		return
	}
	if strings.Contains(message, "required") || strings.Contains(message, "already terminal") ||
		strings.Contains(message, "must be") || strings.Contains(message, "does not belong") {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func invalidParams(err error) *rpcpeer.Error {
	return &rpcpeer.Error{Code: -32602, Message: "invalid params", Data: json.RawMessage(strconv.Quote(err.Error()))}
}

func internalRPCError(err error) *rpcpeer.Error {
	return &rpcpeer.Error{Code: -32603, Message: err.Error()}
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
	})
}
