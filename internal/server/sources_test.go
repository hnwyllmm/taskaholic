package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func TestSourceAPIAuthCASAndUI(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	server := New(Config{APIToken: "test"}, state, nil)
	for _, path := range []string{"/api/v1/sources", "/api/v1/source-targets/a"} {
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if path == "/api/v1/sources" && rec.Code != 401 {
			t.Fatal("source credentials exposed")
		}
	}
	source := model.TaskSource{Kind: "github", Name: "PR watcher", Enabled: true, IntervalSeconds: 5}
	call := func(expected int64, want int) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"source": source, "expected_version": expected})
		req := httptest.NewRequest("PUT", "http://example.com/api/v1/sources/github", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	call(0, 200)
	call(0, 409)
	call(1, 200)
	for _, config := range []string{`{"role_id":"developer"}`, `{"reviewer_role_ids":["reviewer"]}`, `{"project_id":"team"}`, `{"defer_assignment":true}`} {
		req := httptest.NewRequest("PUT", "/api/v1/sources/github", strings.NewReader(`{"source":{"kind":"github","name":"PR","interval_seconds":5,"config":`+config+`},"expected_version":2}`))
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatal("source API accepted execution settings", config, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/sources", "/assets/sources.js", "/assets/sources.css", "/assets/test-pipelines.js"} {
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Fatal("source page missing/security headers", path, rec.Code)
		}
	}
	for _, token := range []string{"", "test"} {
		req := httptest.NewRequest("POST", "/api/v1/work/tasks/t/test-pipelines/p/resolve", strings.NewReader(`{}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		want := 400
		if token == "" {
			want = 401
		}
		if rec.Code != want {
			t.Fatal("pipeline reset requires auth and explicit human confirmation", rec.Code)
		}
	}
	req := httptest.NewRequest("PUT", "http://example.com/api/v1/sources/github", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
}

func TestImportedTaskUsesAIRouterAndUpdatesKeepOriginalNativeSession(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{}, state, nil)
	hello := model.RuntimeHello{RuntimeID: "machine", Epoch: "epoch", Capabilities: map[string]any{
		"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}},
	}}
	if err = state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	s.hub.register(&runtimeConnection{runtimeID: hello.RuntimeID, epoch: hello.Epoch})
	member := func(name, capability string) model.AgentProfile {
		t.Helper()
		draft, err := state.CreateRoleDraft(ctx, name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		draft, err = state.UpdateRoleDraft(ctx, draft.ID, draft.Version, model.RoleSpec{Name: name, Capabilities: []string{capability}, Instructions: "Use evidence", OutputContract: "Deliver evidence"})
		if err != nil {
			t.Fatal(err)
		}
		role, err := state.PublishRoleDraft(ctx, draft.ID, draft.Version)
		if err != nil {
			t.Fatal(err)
		}
		agent, err := state.CreateAgent(ctx, model.AgentProfile{Name: name, RoleID: role.ID, RuntimeID: hello.RuntimeID, AdapterID: "codex-agent", MaxConcurrent: 1})
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}
	dispatcher, worker := member("Router", "analysis"), member("Developer", "code.implement")
	binding, err := state.GetSystemBinding(ctx, "task_router")
	if err != nil {
		t.Fatal(err)
	}
	binding.Mode, binding.AgentID = "agent", dispatcher.ID
	if _, err = state.UpdateSystemBinding(ctx, binding, binding.Version, hello.RuntimeID); err != nil {
		t.Fatal(err)
	}
	source, err := state.SaveTaskSource(ctx, model.TaskSource{ID: "ant", Kind: "antmultica", Name: "Issues", Enabled: true, IntervalSeconds: 5,
		Config: model.SourceConfig{WorkspaceID: "workspace", WorkspaceSlug: "seekdb", AssigneeID: "me", IterationKey: "迭代", IterationValue: "1.5.0"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := state.ListSourceTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event := model.SourceEvent{Key: "issue:1:1", Kind: "antmultica.issue", Entity: "workspace:1", Title: "Implement a requirement", Message: "Implement the SeekDB requirement"}
	if err = state.CommitSourcePoll(ctx, source, targets[0], json.RawMessage(`{"revision":1}`), "", []model.SourceEvent{event}, 0, false); err != nil {
		t.Fatal(err)
	}
	s.scheduleWork(ctx) // The Manager consumes events and queues AI routing.
	tasks, err := state.ListWork(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatal(tasks, err)
	}
	task := tasks[0]
	if !task.Requirements.IsEmpty() || task.AssignedAgentID != "" {
		t.Fatal("source preassigned work", task)
	}
	decision, err := state.GetRoutingDecision(ctx, task.ID, task.Version)
	if err != nil || decision.Router.ID != dispatcher.ID || decision.State != "GENERATING" || len(decision.Candidates) != 2 {
		t.Fatal(decision, err)
	}
	if _, err = state.GetTaskSession(ctx, task.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("source started worker before Router", err)
	}
	output, _ := json.Marshal(map[string]string{"agent_id": worker.ID, "reason": "Development capability matches requirement"})
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 1, RunID: decision.RunID, Type: "run.completed", Output: string(output)}); err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleOne(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	session, err := state.GetTaskSession(ctx, task.ID)
	if err != nil || session.AgentID != worker.ID {
		t.Fatal("AI selection not used", session, err)
	}
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 2, RunID: session.LastRunID, SessionID: session.ID, Type: "session.bound", AgentSessionRef: "codex:original-worker"}); err != nil {
		t.Fatal(err)
	}
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 3, RunID: session.LastRunID, Type: "run.completed", Output: `{"outcome":"needs_input","message":"Please clarify","artifacts":[]}`}); err != nil {
		t.Fatal(err)
	}
	waiting, err := state.GetTask(ctx, task.ID)
	if err != nil || waiting.State != model.TaskStateInput {
		t.Fatal("worker did not reach clarification stage", waiting, err)
	}
	targets, err = state.ListSourceTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event.Key, event.Message = "issue:1:2", "Clarified requirement"
	if err = state.CommitSourcePoll(ctx, source, targets[0], json.RawMessage(`{"revision":2}`), "", []model.SourceEvent{event}, 0, false); err != nil {
		t.Fatal(err)
	}
	if err = state.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleOne(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	continued, err := state.GetTaskSession(ctx, task.ID)
	if err != nil || continued.ID != session.ID || continued.AgentID != worker.ID || continued.AgentSessionRef != "codex:original-worker" || continued.LastRunID == session.LastRunID {
		t.Fatal("source update replaced native memory or failed to continue", continued, err)
	}
}
