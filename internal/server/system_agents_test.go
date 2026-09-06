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

func TestSystemAgentAPIAndSchedulerUseConfiguredMembers(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{APIToken: "test", LocalRuntimeID: "machine", UpgradeEnabled: true}, state, nil)
	call := func(method, path string, body any, want int, target any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		if target != nil {
			if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, method := range []string{"GET", "PUT"} {
		path := "/api/v1/system/agents"
		if method == "PUT" {
			path += "/home_chat"
		}
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != 401 {
			t.Fatal("unauthenticated config", rec.Code)
		}
	}
	req := httptest.NewRequest("PUT", "/api/v1/system/agents/home_chat", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Origin", "https://evil.invalid")
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("cross-origin config", rec.Code)
	}
	hello := model.RuntimeHello{RuntimeID: "machine", Epoch: "epoch", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}}}}
	if err = state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	s.hub.register(&runtimeConnection{runtimeID: hello.RuntimeID, epoch: hello.Epoch})
	draft, err := state.CreateRoleDraft(ctx, "generic", "", "")
	if err != nil {
		t.Fatal(err)
	}
	draft, err = state.UpdateRoleDraft(ctx, draft.ID, draft.Version, model.RoleSpec{Name: "Assistant", Capabilities: []string{"analysis", "document.write"}, Instructions: "Use evidence", OutputContract: "Clear answer", Boundaries: []string{"Respect permissions"}})
	if err != nil {
		t.Fatal(err)
	}
	role, err := state.PublishRoleDraft(ctx, draft.ID, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	a, err := state.CreateAgent(ctx, model.AgentProfile{Name: "Member A", RoleID: role.ID, RuntimeID: hello.RuntimeID, AdapterID: "codex-agent", ModelID: "model-A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := state.CreateAgent(ctx, model.AgentProfile{Name: "Member B", RoleID: role.ID, RuntimeID: hello.RuntimeID, AdapterID: "codex-agent", ModelID: "model-B"})
	if err != nil {
		t.Fatal(err)
	}
	bind := func(slot string, a model.AgentProfile) model.SystemBinding {
		t.Helper()
		current, e := state.GetSystemBinding(ctx, slot)
		if e != nil {
			t.Fatal(e)
		}
		var result model.SystemBinding
		call("PUT", "/api/v1/system/agents/"+slot, map[string]any{"mode": "agent", "agent_id": a.ID, "expected_version": current.Version}, 200, &result)
		return result
	}
	homeBinding := bind("home_chat", a)
	var chat model.HomeChat
	call("POST", "/api/v1/home/chats", map[string]any{}, 201, &chat)
	call("POST", "/api/v1/home/chats/"+chat.ID+"/messages", map[string]any{"message": "hello", "expected_version": chat.Version, "idempotency_key": "hello"}, 202, &chat)
	run, err := state.GetRun(ctx, chat.LastRunID)
	if err != nil || run.AgentID != a.ID || run.ModelID != "model-A" {
		t.Fatal("home configuration ignored", run, err)
	}
	bind("home_chat", b)
	call("POST", "/api/v1/home/chats/"+chat.ID+"/executor", map[string]any{"expected_version": chat.Version, "binding_version": homeBinding.Version}, 409, nil)
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: "machine", Epoch: "epoch", RuntimeSeq: 1, RunID: run.ID, Type: "run.completed", Output: `{"message":"hello","proposal":null}`}); err != nil {
		t.Fatal(err)
	}
	var second model.HomeChat
	call("POST", "/api/v1/home/chats", map[string]any{}, 201, &second)
	call("POST", "/api/v1/home/chats/"+second.ID+"/messages", map[string]any{"message": "second", "expected_version": second.Version}, 202, &second)
	secondRun, err := state.GetRun(ctx, second.LastRunID)
	if err != nil || secondRun.AgentID != b.ID || secondRun.ModelID != "model-B" {
		t.Fatal("new chat did not switch", secondRun, err)
	}
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: "machine", Epoch: "epoch", RuntimeSeq: 2, RunID: secondRun.ID, Type: "run.completed", Output: `{"message":"hello","proposal":null}`}); err != nil {
		t.Fatal(err)
	}
	call("GET", "/api/v1/home/chats/"+chat.ID, nil, 200, &chat)
	call("POST", "/api/v1/home/chats/"+chat.ID+"/messages", map[string]any{"message": "same session", "expected_version": chat.Version}, 202, &chat)
	continued, err := state.GetRun(ctx, chat.LastRunID)
	if err != nil || continued.SessionID != run.SessionID || continued.AgentID != a.ID {
		t.Fatal("old chat switched silently", continued, err)
	}
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: "machine", Epoch: "epoch", RuntimeSeq: 3, RunID: continued.ID, Type: "run.completed", Output: `{"message":"ok","proposal":null}`}); err != nil {
		t.Fatal(err)
	}
	bind("role_builder", b)
	draft, err = state.CreateRoleDraft(ctx, "new role", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var generated struct {
		Run model.Run `json:"run"`
	}
	call("POST", "/api/v1/role-drafts/"+draft.ID+"/messages", map[string]any{"expected_version": draft.Version, "message": "design a role"}, 202, &generated)
	if generated.Run.AgentID != b.ID || generated.Run.ModelID != b.ModelID {
		t.Fatal("role builder ignored slot", generated)
	}
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: "machine", Epoch: "epoch", RuntimeSeq: 4, RunID: generated.Run.ID, Type: "run.failed", Error: "test done"}); err != nil {
		t.Fatal(err)
	}
	bind("task_router", b)
	task, err := state.CreateWork(ctx, store.CreateWorkRequest{Title: "write", Goal: "write a guide", Requirements: model.TaskRequirements{Capabilities: []string{"document.write"}, ExcludedAgentIDs: []string{b.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleOne(ctx, task.ID); !errors.Is(err, model.ErrConflict) {
		t.Fatal("should wait for router", err)
	}
	d, err := state.GetRoutingDecision(ctx, task.ID, task.Version)
	if err != nil || d.Router.ID != b.ID || len(d.Candidates) != 1 || d.Candidates[0] != a.ID {
		t.Fatal("wrong router/candidates", d, err)
	}
	if _, err = state.GetTaskSession(ctx, task.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("business dispatched before decision", err)
	}
	if err = s.scheduleOne(ctx, task.ID); !errors.Is(err, model.ErrConflict) {
		t.Fatal(err)
	}
	again, _ := state.GetRoutingDecision(ctx, task.ID, task.Version)
	if again.RunID != d.RunID {
		t.Fatal("duplicate inference")
	}
	output, _ := json.Marshal(map[string]string{"agent_id": a.ID, "reason": "qualified writer"})
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: "machine", Epoch: "epoch", RuntimeSeq: 5, RunID: d.RunID, Type: "run.completed", Output: string(output)}); err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleOne(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	session, err := state.GetTaskSession(ctx, task.ID)
	if err != nil || session.AgentID != a.ID {
		t.Fatal("did not dispatch choice", session, err)
	}
	// An explicit owner skips inference even when AI routing is enabled.
	explicit, err := state.CreateWork(ctx, store.CreateWorkRequest{Title: "explicit", Goal: "analyze", AgentID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleOne(ctx, explicit.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = state.GetRoutingDecision(ctx, explicit.ID, explicit.Version); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("routed explicit owner", err)
	}
}
