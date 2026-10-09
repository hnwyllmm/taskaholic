package runtimehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"work-assistant/internal/agent"
	"work-assistant/internal/id"
	"work-assistant/internal/model"
	"work-assistant/internal/rpcpeer"
	"work-assistant/internal/workflow"
)

type Config struct {
	RuntimeID  string
	ControlURL string
	Token      string
	WorkRoot   string
}

type Daemon struct {
	config   Config
	spool    *Spool
	log      *slog.Logger
	epoch    string
	hostname string

	adapters          map[string]agent.Adapter
	activeMu          sync.Mutex
	active            map[string]*activeRun
	activeWG          sync.WaitGroup
	storageFault      chan error
	storageErr        error // protected by activeMu; fail closed until process restart
	modelMu           sync.Mutex
	modelCache        map[string]*modelCacheEntry
	preparationMu     sync.Mutex // Serialize shared-base worktree preparation on this runtime.
	windowsGate       chan struct{}
	environmentMu     sync.Mutex
	environmentHealth map[string]windowsHealthCacheEntry
}

type activeRun struct {
	cancel     context.CancelFunc
	directives chan model.Directive
	spec       model.RunSpec
}

func New(config Config, spool *Spool, logger *slog.Logger, adapters ...agent.Adapter) (*Daemon, error) {
	if config.RuntimeID == "" || config.ControlURL == "" || config.WorkRoot == "" {
		return nil, errors.New("runtime-id, control-url and work-root are required")
	}
	if spool == nil {
		return nil, errors.New("runtime spool is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read hostname: %w", err)
	}
	root, err := filepath.Abs(config.WorkRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve work root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create work root: %w", err)
	}
	config.WorkRoot = root
	d := &Daemon{
		config: config, spool: spool, log: logger, epoch: id.New("epoch"), hostname: hostname,
		adapters: make(map[string]agent.Adapter), active: make(map[string]*activeRun),
		storageFault: make(chan error, 1),
	}
	for _, adapter := range adapters {
		if adapter != nil {
			d.adapters[adapter.Name()] = adapter
		}
	}
	if len(d.adapters) == 0 {
		return nil, errors.New("at least one agent adapter is required")
	}
	return d, nil
}

func (d *Daemon) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	go func() {
		select {
		case err := <-d.storageFault:
			cancel(err)
		case <-ctx.Done():
		}
	}()
	stopped := func() error {
		if parent.Err() != nil {
			return nil
		}
		return context.Cause(ctx)
	}
	_, err := d.spool.RecoverActiveRuns(ctx, d.config.RuntimeID, d.epoch)
	if err != nil {
		return fmt.Errorf("recover runtime spool: %w", err)
	}
	defer d.cancelAll()

	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return stopped()
		}
		err := d.runConnection(ctx)
		if ctx.Err() != nil {
			return stopped()
		}
		d.log.Warn("control connection closed", "error", err, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return stopped()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (d *Daemon) runConnection(ctx context.Context) error {
	endpoint, err := runtimeWebSocketURL(d.config.ControlURL)
	if err != nil {
		return err
	}
	header := make(http.Header)
	if d.config.Token != "" {
		header.Set("Authorization", "Bearer "+d.config.Token)
	}
	connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if response != nil {
			return fmt.Errorf("connect control websocket: HTTP %s: %w", response.Status, err)
		}
		return fmt.Errorf("connect control websocket: %w", err)
	}
	defer connection.CloseNow()

	peer := rpcpeer.New(connection, d.handleRequest)
	serveErr := make(chan error, 1)
	go func() { serveErr <- peer.Serve(ctx) }()

	hello := model.RuntimeHello{
		RuntimeID: d.config.RuntimeID, Epoch: d.epoch, Hostname: d.hostname,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Capabilities: d.capabilities(),
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var responseBody struct {
		Accepted bool `json:"accepted"`
	}
	err = peer.Call(callCtx, "runtime.hello", hello, &responseBody)
	cancel()
	if err != nil {
		return fmt.Errorf("runtime hello: %w", err)
	}
	if !responseBody.Accepted {
		return errors.New("control rejected runtime hello")
	}
	d.log.Info("runtime connected", "runtime_id", d.config.RuntimeID, "epoch", d.epoch, "control", d.config.ControlURL)

	sendErr := make(chan error, 1)
	connectionCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { sendErr <- d.sendOutbound(connectionCtx, peer) }()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = peer.Close(websocket.StatusNormalClosure, "runtime shutting down")
			return nil
		case err := <-serveErr:
			return err
		case err := <-sendErr:
			return err
		case <-heartbeat.C:
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			var ack struct {
				Ack bool `json:"ack"`
			}
			err := peer.Call(callCtx, "runtime.heartbeat", map[string]any{"epoch": d.epoch}, &ack)
			cancel()
			if err != nil {
				return fmt.Errorf("runtime heartbeat: %w", err)
			}
		}
	}
}

func (d *Daemon) handleRequest(ctx context.Context, request rpcpeer.Request) (any, *rpcpeer.Error) {
	switch request.Method {
	case "models.list":
		return d.handleListModels(ctx, request)
	case "run.start":
		return d.handleRunStart(ctx, request)
	case "run.interrupt":
		return d.handleRunInterrupt(ctx, request)
	case "run.directive":
		return d.handleRunDirective(ctx, request)
	default:
		return nil, &rpcpeer.Error{Code: -32601, Message: "method not found"}
	}
}

func (d *Daemon) handleRunStart(ctx context.Context, request rpcpeer.Request) (any, *rpcpeer.Error) {
	d.activeMu.Lock()
	storageErr := d.storageErr
	d.activeMu.Unlock()
	if storageErr != nil {
		return nil, rpcInternal(fmt.Errorf("runtime storage is unhealthy: %w", storageErr))
	}
	var spec model.RunSpec
	if err := json.Unmarshal(request.Params, &spec); err != nil {
		return nil, rpcInvalidParams(err)
	}
	if spec.RunID == "" || spec.TaskID == "" || spec.AgentID == "" {
		return nil, rpcInvalidParams(errors.New("run_id, task_id and agent_id are required"))
	}
	if spec.AdapterID == "" {
		// Backward compatibility for v1 outbox messages, where agent_id named the adapter.
		spec.AdapterID = spec.AgentID
	}
	adapter := d.adapters[spec.AdapterID]
	if adapter == nil {
		return nil, rpcInvalidParams(fmt.Errorf("unsupported agent adapter %q", spec.AdapterID))
	}
	for feature, needed := range map[string]bool{"role_instructions": spec.Instructions != "", "structured_output": len(spec.OutputSchema) > 0, "read_only_runs": spec.ReadOnly} {
		if supported, _ := adapter.Capabilities()[feature].(bool); needed && !supported {
			return nil, rpcInvalidParams(fmt.Errorf("adapter %q does not support %s", spec.AdapterID, feature))
		}
	}
	workingDir, err := d.resolveWorkingDir(spec)
	if err != nil {
		return nil, rpcInvalidParams(err)
	}
	duplicate, err := d.spool.AcceptRunStart(ctx, request.ID, request.Params, spec, workingDir)
	if err != nil {
		return nil, rpcInternal(err)
	}
	if duplicate {
		return map[string]any{"accepted": true, "duplicate": true}, nil
	}

	runCtx, cancel := context.WithCancel(context.Background())
	active := &activeRun{cancel: cancel, directives: make(chan model.Directive, 32), spec: spec}
	d.activeMu.Lock()
	d.active[spec.RunID] = active
	d.activeMu.Unlock()
	d.activeWG.Add(1)
	go d.execute(runCtx, request.ID, spec, workingDir, adapter, active)
	return map[string]any{"accepted": true, "duplicate": false}, nil
}

func (d *Daemon) handleRunInterrupt(ctx context.Context, request rpcpeer.Request) (any, *rpcpeer.Error) {
	var command struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(request.Params, &command); err != nil {
		return nil, rpcInvalidParams(err)
	}
	if command.RunID == "" {
		return nil, rpcInvalidParams(errors.New("run_id is required"))
	}
	duplicate, err := d.spool.AcceptInbound(ctx, request.ID, request.Method, request.Params)
	if err != nil {
		return nil, rpcInternal(err)
	}
	if duplicate {
		return map[string]any{"accepted": true, "duplicate": true}, nil
	}
	d.activeMu.Lock()
	active := d.active[command.RunID]
	d.activeMu.Unlock()
	if active != nil {
		active.cancel()
	}
	_ = d.spool.SetInboundStatus(context.Background(), request.ID, "APPLIED", map[string]any{"interrupted": active != nil})
	return map[string]any{"accepted": true, "applied": active != nil}, nil
}

func (d *Daemon) handleRunDirective(ctx context.Context, request rpcpeer.Request) (any, *rpcpeer.Error) {
	var directive model.Directive
	if err := json.Unmarshal(request.Params, &directive); err != nil {
		return nil, rpcInvalidParams(err)
	}
	if directive.ID == "" || directive.RunID == "" || directive.TaskID == "" || directive.Kind == "" {
		return nil, rpcInvalidParams(errors.New("directive_id, run_id, task_id and kind are required"))
	}
	duplicate, err := d.spool.AcceptInbound(ctx, request.ID, request.Method, request.Params)
	if err != nil {
		return nil, rpcInternal(err)
	}
	if duplicate {
		return map[string]any{"accepted": true, "duplicate": true}, nil
	}
	d.activeMu.Lock()
	active := d.active[directive.RunID]
	d.activeMu.Unlock()
	if active == nil || active.spec.TaskID != directive.TaskID || active.spec.SessionID != directive.SessionID {
		d.emitDirectiveResult(directive, "directive.rejected", "run is not active on this runtime")
		_ = d.spool.SetInboundStatus(context.Background(), request.ID, "REJECTED", nil)
		return map[string]any{"accepted": true, "applied": false}, nil
	}
	if directive.Kind == model.DirectiveKindInterrupt {
		active.cancel()
		d.emitDirectiveResult(directive, "directive.applied", "")
		_ = d.spool.SetInboundStatus(context.Background(), request.ID, "APPLIED", nil)
		return map[string]any{"accepted": true, "applied": true}, nil
	}
	select {
	case active.directives <- directive:
		_ = d.spool.SetInboundStatus(context.Background(), request.ID, "DELIVERED", nil)
		return map[string]any{"accepted": true, "delivered": true}, nil
	case <-ctx.Done():
		return nil, rpcInternal(ctx.Err())
	default:
		d.emitDirectiveResult(directive, "directive.rejected", "agent directive queue is full")
		_ = d.spool.SetInboundStatus(context.Background(), request.ID, "REJECTED", nil)
		return map[string]any{"accepted": true, "delivered": false}, nil
	}
}

func (d *Daemon) execute(ctx context.Context, messageID string, spec model.RunSpec, workingDir string, adapter agent.Adapter, active *activeRun) {
	defer d.activeWG.Done()
	defer func() {
		active.cancel()
		d.activeMu.Lock()
		if d.active[spec.RunID] == active {
			delete(d.active, spec.RunID)
		}
		d.activeMu.Unlock()
	}()
	if err := d.spool.SetRunState(context.Background(), spec.RunID, "RUNNING"); err != nil {
		d.failStorage(err)
		return
	}
	agentSessionRef := spec.AgentSessionRef
	if agentSessionRef == "" {
		agentSessionRef = "workspace:" + workingDir
	}
	if spec.SessionID != "" {
		if err := d.emit(context.Background(), model.RuntimeEvent{
			RuntimeID: d.config.RuntimeID, Epoch: d.epoch, RunID: spec.RunID, TaskID: spec.TaskID,
			SessionID: spec.SessionID, Type: "session.bound", AgentSessionRef: agentSessionRef,
			CausationID: messageID,
		}); err != nil {
			return
		}
	}
	if err := d.emit(context.Background(), model.RuntimeEvent{
		RuntimeID: d.config.RuntimeID, Epoch: d.epoch, RunID: spec.RunID, TaskID: spec.TaskID,
		SessionID: spec.SessionID, Type: "run.started", CausationID: messageID,
		Attributes: map[string]any{"adapter": adapter.Name(), "agent_id": spec.AgentID, "working_dir": workingDir},
	}); err != nil {
		return
	}

	var workspace developmentWorkspace
	var receipt string
	var preparationErr error
	agentWorkingDir := workingDir
	if spec.ExecutionGrant != nil {
		workspace, receipt, preparationErr = d.prepareDevelopment(ctx, spec, workingDir)
		if preparationErr == nil {
			agentWorkingDir = workspace.Directory
			spec.Instructions += "\n本轮已批准隔离源码目录：" + workspace.Directory + "\n只在该目录修改代码。Git 元数据由 runtime 维护；不得修改 .git、调用外部发布或更改批准范围。代码和验证完成后，publish_request={title:PR标题,body:实现说明、测试证据和风险}，由 runtime 提交到任务专用分支并创建 PR。plan_scope=null。"
		}
	}
	emit := func(event agent.Event) {
		eventType := event.Type
		if eventType == "" {
			eventType = "run.progress"
		}
		causationID := messageID
		if event.DirectiveID != "" {
			causationID = event.DirectiveID
		}
		d.emit(context.Background(), model.RuntimeEvent{
			RuntimeID: d.config.RuntimeID, Epoch: d.epoch, RunID: spec.RunID, TaskID: spec.TaskID,
			SessionID: spec.SessionID, DirectiveID: event.DirectiveID, Type: eventType,
			AgentSessionRef: event.AgentSessionRef, Message: event.Message,
			Stream: event.Stream, Error: event.Error, CausationID: causationID,
			Activity:   event.Activity,
			Execution:  event.Execution,
			Usage:      event.Usage,
			Attributes: event.Attributes,
		})
	}
	result := agent.Result{ExitCode: -1, Err: preparationErr}
	if spec.Environment != nil {
		result = d.executeEnvironment(ctx, spec, emit)
	} else if preparationErr == nil {
		instructions, stopBridge, bridgeErr := d.startVMBridge(ctx, spec, workingDir, workspace.Directory, emit)
		if bridgeErr != nil {
			result = agent.Result{ExitCode: -1, Err: bridgeErr}
		} else {
			if instructions != "" {
				spec.AdditionalWritableRoots = append(spec.AdditionalWritableRoots, filepath.Join(workingDir, ".assistant-vm-"+spec.RunID))
			}
			spec.Instructions += instructions
			result = adapter.Run(ctx, spec, agentWorkingDir, active.directives, emit)
		}
		stopBridge()
	}
	if result.Err == nil && spec.ExecutionGrant != nil && ctx.Err() == nil {
		parsed, err := workflow.Parse(result.Output)
		if err == nil && parsed.PublishRequest != nil {
			if err = publishDevelopment(ctx, &workspace, receipt, spec, &parsed); err != nil {
				parsed.Outcome = "blocked"
				parsed.PublishRequest = nil
				parsed.Message += "\n受控 PR 发布未完成：" + err.Error()
				if parsed.TaskUpdate != nil {
					parsed.TaskUpdate.BlockedReason = err.Error()
				}
			}
			raw, marshalErr := json.Marshal(parsed)
			if marshalErr != nil {
				result.Err = marshalErr
			} else {
				result.Output = string(raw)
			}
		}
	}
	exitCode := result.ExitCode
	terminal := model.RuntimeEvent{
		Output:    result.Output,
		RuntimeID: d.config.RuntimeID, Epoch: d.epoch, RunID: spec.RunID, TaskID: spec.TaskID,
		SessionID: spec.SessionID, ExitCode: &exitCode, CausationID: messageID,
	}
	state := "COMPLETED"
	switch {
	case ctx.Err() != nil:
		terminal.Type = "run.interrupted"
		terminal.Error = ctx.Err().Error()
		state = "INTERRUPTED"
	case result.Err != nil:
		terminal.Type = "run.failed"
		terminal.Error = result.Err.Error()
		state = "FAILED"
	default:
		terminal.Type = "run.completed"
	}
	// Retain the output while retrying transient persistence failures. If the
	// process must exit, leave the run recoverable, never falsely completed.
	persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if err := d.spool.CompleteRun(persistCtx, messageID, state, terminal); err == nil {
			break
		} else {
			d.log.Error("terminal result not durable; run remains recoverable", "run_id", spec.RunID, "error", err)
		}
		select {
		case <-persistCtx.Done():
			d.failStorage(fmt.Errorf("could not persist terminal result for %s: %w", spec.RunID, persistCtx.Err()))
			return
		case <-time.After(time.Second):
		}
	}
}

func (d *Daemon) emitDirectiveResult(directive model.Directive, eventType, reason string) {
	d.emit(context.Background(), model.RuntimeEvent{
		RuntimeID: d.config.RuntimeID, Epoch: d.epoch, RunID: directive.RunID, TaskID: directive.TaskID,
		SessionID: directive.SessionID, DirectiveID: directive.ID, Type: eventType,
		Error: reason, CausationID: directive.ID,
	})
}

func (d *Daemon) sendOutbound(ctx context.Context, peer *rpcpeer.Peer) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		message, err := d.spool.NextOutbound(ctx)
		if err != nil {
			return fmt.Errorf("read runtime outbox: %w", err)
		}
		if message == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var response struct {
			Durable bool `json:"durable"`
		}
		err = peer.CallID(callCtx, message.MessageID, message.Method, message.Params, &response)
		cancel()
		if err != nil {
			_ = d.spool.RecordOutboundError(context.Background(), message.RuntimeSeq, err.Error())
			return fmt.Errorf("deliver runtime event %d: %w", message.RuntimeSeq, err)
		}
		if !response.Durable {
			return fmt.Errorf("control did not durably accept runtime event %d", message.RuntimeSeq)
		}
		if err := d.spool.MarkOutboundDelivered(context.Background(), message.RuntimeSeq); err != nil {
			return fmt.Errorf("mark runtime event delivered: %w", err)
		}
	}
}

func (d *Daemon) emit(ctx context.Context, event model.RuntimeEvent) error {
	if _, err := d.spool.EnqueueEvent(ctx, event); err != nil {
		d.log.Error("persist runtime event", "run_id", event.RunID, "type", event.Type, "error", err)
		d.failStorage(err)
		return err
	}
	return nil
}

func (d *Daemon) failStorage(err error) {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	if d.storageErr != nil {
		return
	}
	d.storageErr = err
	d.storageFault <- err
}

func (d *Daemon) resolveWorkingDir(spec model.RunSpec) (string, error) {
	path := spec.WorkingDir
	if path == "" {
		if spec.SessionID != "" {
			path = filepath.Join(d.config.WorkRoot, "sessions", spec.SessionID)
		} else {
			path = filepath.Join(d.config.WorkRoot, "runs", spec.RunID)
		}
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(d.config.WorkRoot, path)
	}
	path = filepath.Clean(path)
	relative, err := filepath.Rel(d.config.WorkRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("working directory must stay under work root %s", d.config.WorkRoot)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("create working directory: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(d.config.WorkRoot)
	if err != nil {
		return "", fmt.Errorf("resolve work root symlinks: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve working directory symlinks: %w", err)
	}
	relative, err = filepath.Rel(realRoot, realPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("working directory escapes work root through a symlink")
	}
	return realPath, nil
}

func (d *Daemon) capabilities() map[string]any {
	result := map[string]any{
		"executors":      d.executionCapabilities(),
		"protocol":       "json-rpc-2.0/websocket",
		"durable_spool":  true,
		"workspace_root": d.config.WorkRoot,
		"adapters":       map[string]any{},
	}
	adapterCaps := result["adapters"].(map[string]any)
	for name, adapter := range d.adapters {
		caps := make(map[string]any)
		for key, value := range adapter.Capabilities() {
			caps[key] = value
		}
		_, caps["model_catalog"] = adapter.(agent.ModelProvider)
		caps["approved_development"] = name == "codex-agent"
		// A development-capable runtime must also advertise that it has the
		// controlled publisher hook. This prevents a control-plane upgrade from
		// silently dispatching publish work to a stale long-lived runtime.
		caps["controlled_publication"] = name == "codex-agent"
		adapterCaps[name] = caps
	}
	return result
}

func (d *Daemon) cancelAll() {
	d.activeMu.Lock()
	active := make([]*activeRun, 0, len(d.active))
	for _, run := range d.active {
		active = append(active, run)
	}
	d.activeMu.Unlock()
	for _, run := range active {
		run.cancel()
	}
	d.activeWG.Wait()
}

func runtimeWebSocketURL(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse control URL: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported control URL scheme %q", parsed.Scheme)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/runtime/ws"
	return parsed.String(), nil
}

func rpcInvalidParams(err error) *rpcpeer.Error {
	encoded, _ := json.Marshal(err.Error())
	return &rpcpeer.Error{Code: -32602, Message: "invalid params", Data: encoded}
}

func rpcInternal(err error) *rpcpeer.Error {
	return &rpcpeer.Error{Code: -32603, Message: err.Error()}
}
