package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/rolebuilder"
	"work-assistant/internal/store"
)

func TestRoleStudioAPIAndRoleRouting(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{APIToken: "test-api-token"}, state, nil)
	call := func(method, path string, body any, status int, target any) {
		t.Helper()
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer test-api-token")
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s = %d: %s", method, path, rec.Code, rec.Body.String())
		}
		if target != nil {
			if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	unauthorized := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(unauthorized, httptest.NewRequest("GET", "/api/v1/roles", nil))
	if unauthorized.Code != 401 {
		t.Fatal("roles API has no auth")
	}
	page := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(page, httptest.NewRequest("GET", "/roles", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "成员管理") || page.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("role UI missing or unsafe")
	}
	call("POST", "/api/v1/role-drafts", map[string]any{"unknown": true}, 400, nil)
	var draft model.RoleDraft
	call("POST", "/api/v1/role-drafts", map[string]any{"description": "a reviewer"}, 201, &draft)
	message := map[string]any{"message": "Design a review role", "expected_version": draft.Version, "model_id": "test-model", "idempotency_key": "message-1"}
	call("POST", "/api/v1/role-drafts/"+draft.ID+"/messages", message, 409, nil)
	unchanged, err := state.GetRoleDraft(ctx, draft.ID)
	if err != nil || unchanged.State != "DRAFT" || len(unchanged.Messages) != 0 {
		t.Fatal("offline generation mutated draft")
	}
	hello := model.RuntimeHello{RuntimeID: "runtime", Epoch: "epoch", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"structured_output": true, "role_instructions": true, "read_only_runs": true}}}}
	if err := state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	s.hub.register(&runtimeConnection{runtimeID: hello.RuntimeID, epoch: hello.Epoch})
	old := model.RuntimeHello{RuntimeID: "a-old-runtime", Epoch: "old-epoch", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{}}}}
	if err := state.RegisterRuntime(ctx, old); err != nil {
		t.Fatal(err)
	}
	s.hub.register(&runtimeConnection{runtimeID: old.RuntimeID, epoch: old.Epoch})
	var generation struct {
		Draft model.RoleDraft `json:"draft"`
		Run   model.Run       `json:"run"`
	}
	call("POST", "/api/v1/role-drafts/"+draft.ID+"/messages", message, 202, &generation)
	if generation.Run.RuntimeID != hello.RuntimeID {
		t.Fatal("builder chose an old runtime without required features")
	}
	call("POST", "/api/v1/role-drafts/"+draft.ID+"/messages", message, 200, nil)
	call("PUT", "/api/v1/role-drafts/"+draft.ID, map[string]any{"expected_version": generation.Draft.Version, "spec": model.RoleSpec{}}, 409, nil)
	output, _ := json.Marshal(rolebuilder.Reply{Message: "ready", Questions: []string{}, Draft: model.RoleSpec{Name: "Reviewer", Description: "for display", Capabilities: []string{"code.review"}, Instructions: "Review code without editing.", OutputContract: "Evidence and findings", Boundaries: []string{"Do not merge"}}})
	if _, err := state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 1, RunID: generation.Run.ID, Type: "run.completed", Output: string(output)}); err != nil {
		t.Fatal(err)
	}
	call("GET", "/api/v1/role-drafts/"+draft.ID, nil, 200, &draft)
	var role model.Role
	call("POST", "/api/v1/role-drafts/"+draft.ID+"/publish", map[string]any{"expected_version": draft.Version}, 200, &role)
	var agent model.AgentProfile
	call("POST", "/api/v1/agents", map[string]any{"name": "reviewer-1", "role_id": role.ID, "runtime_id": "runtime", "model_id": "test-model"}, 201, &agent)
	call("POST", "/api/v1/agents", map[string]any{"name": "reviewer-1", "role_id": role.ID, "runtime_id": "runtime"}, 409, nil)
	var task model.Task
	call("POST", "/api/v1/tasks", map[string]any{"title": "review change", "goal": "Review the proposal", "requirements": model.TaskRequirements{Capabilities: []string{"code.review"}}}, 201, &task)
	var run model.Run
	call("POST", "/api/v1/tasks/"+task.ID+"/runs", map[string]any{}, 201, &run)
	if run.AgentID != agent.ID || run.ModelID != "test-model" || run.Role == nil || run.Role.ID != role.ID {
		t.Fatalf("routing lost role: %+v", run)
	}
	call("POST", "/api/v1/tasks/"+task.ID+"/runs", map[string]any{"agent_id": "some-other-agent"}, 409, nil)
	if _, err := state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 2, RunID: run.ID, Type: "run.completed", Output: "LGTM"}); err != nil {
		t.Fatal(err)
	}
	var resumed model.Run
	call("POST", "/api/v1/tasks/"+task.ID+"/runs", map[string]any{}, 201, &resumed)
	if resumed.SessionID != run.SessionID {
		t.Fatal("resume changed session")
	}
	var child model.SubtaskResult
	call("POST", "/api/v1/tasks/"+task.ID+"/subtasks", map[string]any{"title": "security review", "goal": "Review security", "requirements": model.TaskRequirements{Capabilities: []string{"security.review"}, ExcludedAgentIDs: []string{agent.ID}}}, 201, &child)
	call("POST", "/api/v1/tasks/"+child.Task.ID+"/runs", map[string]any{}, 409, nil)
	call("GET", "/api/v1/tasks/"+child.Task.ID, nil, http.StatusOK, nil)
	s.hub.unregister(hello.RuntimeID, nil)
	call("POST", "/api/v1/role-drafts/"+draft.ID+"/messages", message, 200, nil)
}
