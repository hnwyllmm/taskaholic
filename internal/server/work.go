package server

import (
	"context"
	"database/sql"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/router"
	"work-assistant/internal/store"
)

func (s *Server) registerWorkRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/work/tasks/{task_id}/development/retry", s.apiAuth(http.HandlerFunc(s.handleDevelopmentRetry)))
	mux.Handle("POST /api/v1/work/tasks/{task_id}/development/restart", s.apiAuth(http.HandlerFunc(s.handleDevelopmentRestart)))
	mux.Handle("POST /api/v1/work/tasks/{task_id}/test-pipelines/{request_id}/resolve", s.apiAuth(http.HandlerFunc(s.handleResolveTestPipeline)))
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/work/tasks":                                                        s.handleWorkList,
		"GET /api/v1/work/summaries":                                                    s.handleWorkSummaries,
		"POST /api/v1/work/tasks":                                                       s.handleWorkCreate,
		"GET /api/v1/work/tasks/{task_id}":                                              s.handleWorkDetail,
		"GET /api/v1/work/tasks/{task_id}/hierarchy":                                    s.handleWorkHierarchy,
		"POST /api/v1/work/tasks/{task_id}/assignment":                                  s.handleWorkAssignment,
		"GET /api/v1/work/tasks/{task_id}/activities":                                   s.handleActivities,
		"GET /api/v1/work/tasks/{task_id}/events":                                       s.handleEventStream,
		"POST /api/v1/work/tasks/{task_id}/messages":                                    s.handleWorkMessage,
		"POST /api/v1/work/tasks/{task_id}/pause":                                       s.handleWorkPause,
		"POST /api/v1/work/tasks/{task_id}/reviews/{review_id}":                         s.handleWorkReview,
		"POST /api/v1/work/tasks/{task_id}/reviews/{review_id}/messages":                s.handleReviewMessage,
		"POST /api/v1/work/tasks/{task_id}/reviews/{review_id}/messages/{turn_id}/stop": s.handleReviewStop,
		"GET /api/v1/work/tasks/{task_id}/artifacts/{artifact_id}":                      s.handleWorkArtifact,
		"GET /api/v1/projects":                                                          s.handleProjects,
		"POST /api/v1/projects":                                                         s.handleCreateProject,
		"GET /api/v1/system":                                                            s.handleSystem,
	} {
		mux.Handle(pattern, s.apiAuth(handler))
	}
}

func (s *Server) handleDevelopmentRetry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectedVersion int64 `json:"expected_version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	m, err := s.store.RetryDevelopment(r.Context(), r.PathValue("task_id"), req.ExpectedVersion)
	reply(w, http.StatusAccepted, m, err)
}

func (s *Server) handleWorkList(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("scope") {
	case "roots":
		tasks, err := s.store.ListRootWork(r.Context())
		reply(w, 200, map[string]any{"tasks": tasks}, err)
		return
	case "", "all":
		// Preserve existing callers such as the home inbox and PR registration.
	default:
		writeError(w, 400, fmt.Errorf("%w: scope must be roots or all", model.ErrValidation))
		return
	}
	tasks, err := s.store.ListWork(r.Context())
	reply(w, 200, map[string]any{"tasks": tasks}, err)
}
func (s *Server) handleWorkHierarchy(w http.ResponseWriter, r *http.Request) {
	hierarchy, err := s.store.GetWorkHierarchy(r.Context(), r.PathValue("task_id"))
	reply(w, 200, hierarchy, err)
}
func (s *Server) handleWorkSummaries(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	summaries, err := s.store.ListTaskSummaries(r.Context(), limit)
	reply(w, 200, map[string]any{"summaries": summaries}, err)
}
func (s *Server) handleWorkCreate(w http.ResponseWriter, r *http.Request) {
	var req store.CreateWorkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	if req.Key == "" {
		req.Key = r.Header.Get("Idempotency-Key")
	}
	task, err := s.store.CreateWork(r.Context(), req)
	reply(w, 201, task, err)
}
func (s *Server) handleWorkAssignment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID         string `json:"agent_id"`
		ExpectedVersion int64  `json:"expected_version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	task, err := s.store.AssignWork(r.Context(), r.PathValue("task_id"), req.AgentID, req.ExpectedVersion)
	reply(w, http.StatusAccepted, task, err)
}
func (s *Server) handleWorkDetail(w http.ResponseWriter, r *http.Request) {
	work, err := s.store.GetWorkDetail(r.Context(), r.PathValue("task_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("event_limit"))
	detail, err := s.store.GetTaskDetail(r.Context(), r.PathValue("task_id"), limit)
	reply(w, 200, map[string]any{"detail": detail, "work": work}, err)
}
func (s *Server) handleWorkMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message   string `json:"message"`
		Key       string `json:"idempotency_key"`
		Interrupt bool   `json:"interrupt"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	if req.Key == "" {
		req.Key = r.Header.Get("Idempotency-Key")
	}
	m, err := s.store.MessageWork(r.Context(), r.PathValue("task_id"), req.Message, req.Key, req.Interrupt)
	reply(w, 202, m, err)
}
func (s *Server) handleWorkPause(w http.ResponseWriter, r *http.Request) {
	err := s.store.PauseWork(r.Context(), r.PathValue("task_id"))
	reply(w, 202, map[string]any{"status": "PAUSED"}, err)
}
func (s *Server) handleWorkReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DiscussionVersion int64  `json:"expected_discussion_version"`
		Decision          string `json:"decision"`
		Comment           string `json:"comment"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	review, err := s.store.DecideReview(r.Context(), r.PathValue("task_id"), r.PathValue("review_id"), req.Decision, req.Comment, req.DiscussionVersion)
	reply(w, 200, review, err)
}
func (s *Server) handleWorkArtifact(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.GetArtifact(r.Context(), r.PathValue("task_id"), r.PathValue("artifact_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}))
	w.Header().Set("ETag", `"`+a.SHA256+`"`)
	_, _ = w.Write([]byte(a.Content))
}
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.ListProjects(r.Context())
	reply(w, 200, map[string]any{"projects": p}, err)
}
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Context string `json:"context"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	p, err := s.store.CreateProject(r.Context(), req.Name, req.Context)
	reply(w, 201, p, err)
}
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	runtimes, err := s.connectedRuntimes(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ready := 0
	for _, a := range agents {
		for _, rt := range runtimes {
			if a.RuntimeID == rt.ID && a.State == "ACTIVE" && router.SupportsFeature(rt, a.AdapterID, "structured_output") && router.SupportsFeature(rt, a.AdapterID, "read_only_runs") {
				ready++
			}
		}
	}
	maintenance, _ := s.store.Maintenance(r.Context())
	var protection any = map[string]any{"enabled": false}
	if s.config.Backups != nil {
		protection = s.config.Backups.Status()
	}
	reply(w, 200, map[string]any{"database": "ready", "scheduler": "enabled", "connected_runtimes": len(runtimes), "eligible_agents": ready, "runtimes": runtimes, "agents": agents, "execution_mode": "read-only; UTF-8 artifacts stored in local SQLite", "model_validation": "configuration only; a real task verifies authentication and model availability", "upgrade_enabled": s.config.UpgradeEnabled, "upgrade_validation_sandbox": s.config.UpgradeValidationSandbox, "maintenance_upgrade_id": maintenance, "data_protection": protection}, nil)
}

func (s *Server) workLoop(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scheduleWork(ctx)
		}
	}
}
func (s *Server) scheduleWork(ctx context.Context) {
	if err := s.store.RoutePlanReviews(ctx); err != nil {
		s.log.Error("route plan reviews", "error", err)
	}
	// Manager work is independent of polling latency or source enablement.
	// Sources cannot invoke routing, fan-out, fan-in, or start an Agent.
	if err := s.store.CollectSourceReviews(ctx); err != nil {
		s.log.Error("collect PR review results", "error", err)
	}
	if err := s.store.ProcessSourceEvents(ctx, s.reviewPlanner); err != nil {
		s.log.Error("process task events", "error", err)
	}
	s.scheduleReviewTurns(ctx)
	ids, err := s.store.PendingWork(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("read scheduling queue", "error", err)
		}
		return
	}
	for _, taskID := range ids {
		if err := s.scheduleOne(ctx, taskID); err != nil {
			if e := s.store.DeferWork(ctx, taskID, err); e != nil && ctx.Err() == nil {
				s.log.Error("persist scheduling error", "error", e)
			}
		}
	}
}
func (s *Server) handleDevelopmentRestart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectedVersion int64 `json:"expected_version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	d, err := s.store.RestartDevelopment(r.Context(), r.PathValue("task_id"), req.ExpectedVersion)
	reply(w, http.StatusAccepted, d, err)
}
func (s *Server) scheduleOne(ctx context.Context, taskID string) error {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	config, err := s.store.GetWorkConfig(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State == model.TaskStateNew || config.Paused {
		return nil
	}
	runtimes, err := s.connectedRuntimes(ctx)
	if err != nil {
		return err
	}
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return err
	}
	// Filter unsupported adapters before selection, so an older runtime cannot
	// repeatedly win and starve a task that another runtime could handle.
	eligible := []model.AgentProfile{}
	for _, a := range agents {
		for _, rt := range runtimes {
			if rt.ID == a.RuntimeID && router.SupportsFeature(rt, a.AdapterID, "structured_output") && router.SupportsFeature(rt, a.AdapterID, "read_only_runs") {
				eligible = append(eligible, a)
				break
			}
		}
	}
	req := store.CreateRunRequest{TaskID: taskID, ExpectedTaskVersion: task.Version}
	if session, e := s.store.GetTaskSession(ctx, taskID); e == nil {
		// Affinity is stronger than current configuration. Never fail over an
		// existing task to another employee or lose its native session.
		req.SessionID, req.AgentID, req.RuntimeID, req.AdapterID, req.ModelID = session.ID, session.AgentID, session.RuntimeID, session.AdapterID, session.ModelID
		found := false
		for _, a := range eligible {
			if a.ID == session.AgentID && a.RuntimeID == session.RuntimeID {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%w: original agent/runtime unavailable; waiting without changing session", model.ErrConflict)
		}
	} else if e != sql.ErrNoRows {
		return e
	} else {
		selectedID := config.AgentID
		if selectedID == "" {
			d, e := s.store.GetRoutingDecision(ctx, taskID, task.Version)
			if e != nil && e != sql.ErrNoRows {
				return e
			}
			binding, e2 := s.store.GetSystemBinding(ctx, "task_router")
			if e2 != nil {
				return e2
			}
			if e == nil && (d.State != "FAILED" || binding.Version == d.Binding.Version) {
				if d.State == "GENERATING" {
					return fmt.Errorf("%w: 路由成员 %s 正在选择执行者", model.ErrConflict, d.Router.Name)
				}
				if d.State != "READY" {
					return fmt.Errorf("%w: AI 路由失败：%s；可追加任务消息重试，或切回规则路由", model.ErrConflict, d.Error)
				}
				selectedID = d.AgentID
				req.AssignmentDecisionID = d.ID
			} else if binding.Mode == "agent" {
				if e == nil {
					return fmt.Errorf("%w: 上次 AI 路由失败，请追加任务消息以发起新一轮路由", model.ErrConflict)
				}
				executor, e := s.systemExecutor(ctx, "task_router", "", "")
				if e != nil {
					return e
				}
				a, e := s.store.GetAgent(ctx, executor.AgentID)
				if e != nil {
					return e
				}
				pool := []model.AgentProfile{}
				for _, candidate := range eligible {
					if candidate.State == "ACTIVE" && candidate.ActiveRuns < candidate.MaxConcurrent && router.Matches(task.Requirements, candidate) {
						pool = append(pool, candidate)
					}
				}
				_, e = s.store.StartRoutingDecision(ctx, taskID, task.Version, *executor.SystemBinding, a, pool)
				if e != nil {
					return e
				}
				return fmt.Errorf("%w: AI 路由已排队，等待选择执行者", model.ErrConflict)
			}
		}
		a, e := s.agentRouter.SelectAgent(ctx, router.AgentRequest{Requirements: task.Requirements, AgentID: selectedID}, eligible, runtimes)
		if e != nil {
			return e
		}
		req.AgentID, req.RuntimeID, req.AdapterID, req.ModelID = a.ID, a.RuntimeID, a.AdapterID, a.ModelID
	}
	d, devErr := s.store.GetDevelopment(ctx, taskID)
	if devErr != nil && devErr != sql.ErrNoRows {
		return devErr
	}
	if devErr == nil && d.Phase == "IMPLEMENTING" {
		supported := false
		for _, rt := range runtimes {
			if rt.ID == req.RuntimeID && router.SupportsFeature(rt, req.AdapterID, "approved_development") {
				supported = true
			}
		}
		if !supported {
			return fmt.Errorf("%w: 原 runtime/adapter 尚不支持已审批隔离开发，请先升级；不会更换原 Session", model.ErrConflict)
		}
	}
	_, err = s.store.StartWorkRun(ctx, req, s.workContract)
	return err
}
