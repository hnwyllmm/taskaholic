package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func TestAssignmentAPIManualAutomaticAndSavedTaskAttention(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{APIToken: "test"}, state, nil)
	hello := model.RuntimeHello{RuntimeID: "machine", Epoch: "epoch", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}}}}
	if err = state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	s.hub.register(&runtimeConnection{runtimeID: hello.RuntimeID, epoch: hello.Epoch})
	draft, err := state.CreateRoleDraft(ctx, "writer", "", "")
	if err != nil {
		t.Fatal(err)
	}
	draft, err = state.UpdateRoleDraft(ctx, draft.ID, draft.Version, model.RoleSpec{Name: "writer", Capabilities: []string{"document.write"}, Instructions: "Write documents", OutputContract: "Full text files"})
	if err != nil {
		t.Fatal(err)
	}
	role, err := state.PublishRoleDraft(ctx, draft.ID, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	var agents []model.AgentProfile
	for _, name := range []string{"writer-a", "writer-b"} {
		a, err := state.CreateAgent(ctx, model.AgentProfile{Name: name, RoleID: role.ID, RuntimeID: hello.RuntimeID, AdapterID: "codex-agent", MaxConcurrent: 1})
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, a)
	}
	call := func(path string, body any, want int, target any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/api/v1/work/tasks"+path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if target != nil {
			if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	create := func() model.Task {
		var task model.Task
		call("", map[string]any{"title": "Choose executor", "goal": "Write a guide", "defer_assignment": true}, 201, &task)
		return task
	}
	task := create()
	s.scheduleWork(ctx)
	if err = s.scheduleOne(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	detail, _ := state.GetTaskDetail(ctx, task.ID)
	if task.State != model.TaskStateNew || len(detail.Runs) != 0 {
		t.Fatal("saved task started prematurely", detail)
	}
	r := httptest.NewRequest("GET", "/api/v1/home", nil)
	r.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, r)
	var home struct {
		Counts    map[string]int `json:"counts"`
		Attention []struct {
			Task model.Task `json:"task"`
		} `json:"attention"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &home); err != nil || home.Counts[model.TaskStateNew] != 1 || len(home.Attention) != 1 || home.Attention[0].Task.ID != task.ID {
		t.Fatal("unassigned task missing from home", home, err)
	}
	endpoint := "/" + task.ID + "/assignment"
	for _, origin := range []string{"", "https://evil.invalid"} {
		r := httptest.NewRequest("POST", "/api/v1/work/tasks"+endpoint, bytes.NewBufferString(`{}`))
		want := 401
		if origin != "" {
			want = 403
			r.Header.Set("Origin", origin)
			r.Header.Set("Authorization", "Bearer test")
		}
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal("assignment auth missing", w.Code)
		}
	}
	call(endpoint, map[string]any{"agent_id": agents[1].ID}, 400, nil)
	call(endpoint, map[string]any{"agent_id": "missing", "expected_version": task.Version}, 404, nil)
	call(endpoint, map[string]any{"agent_id": agents[1].ID, "expected_version": task.Version}, 202, &task)
	if err = s.scheduleOne(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	detail, _ = state.GetTaskDetail(ctx, task.ID)
	if len(detail.Runs) != 1 || detail.Runs[0].AgentID != agents[1].ID {
		t.Fatal("manual selection ignored", detail.Runs)
	}
	call(endpoint, map[string]any{"agent_id": agents[0].ID, "expected_version": detail.Task.Version}, 409, nil)

	auto := create()
	call("/"+auto.ID+"/assignment", map[string]any{"agent_id": "", "expected_version": auto.Version}, 202, &auto)
	if err = s.scheduleOne(ctx, auto.ID); err != nil {
		t.Fatal(err)
	}
	detail, _ = state.GetTaskDetail(ctx, auto.ID)
	if len(detail.Runs) != 1 || detail.Runs[0].AgentID != agents[0].ID {
		t.Fatal("automatic route did not select available member", detail.Runs)
	}
}
