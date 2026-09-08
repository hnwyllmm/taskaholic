package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func TestTaskConsultationAPIUsesIndependentRun(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	task, err := state.CreateWork(ctx, store.CreateWorkRequest{Title: "Investigate", Goal: "Explain and fix", DeferAssignment: true, Key: "subject"})
	if err != nil {
		t.Fatal(err)
	}
	hello := model.RuntimeHello{RuntimeID: "runtime", Epoch: "epoch", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "read_only_runs": true, "structured_output": true}}}}
	if err = state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	s := New(Config{APIToken: "test"}, state, nil)
	s.hub.register(&runtimeConnection{runtimeID: hello.RuntimeID, epoch: hello.Epoch})
	call := func(method, path string, body any, want int, target any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if target != nil {
			if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	base := "/api/v1/work/tasks/" + task.ID + "/consultation"
	call("GET", base, nil, 200, nil)
	var item model.TaskConsultation
	call("POST", base, map[string]any{}, 201, &item)
	call("POST", base+"/messages", map[string]any{"message": "现在在做什么？", "expected_version": item.Version, "idempotency_key": "q1"}, 202, &item)
	if item.ExecutionTaskID == task.ID || item.State != "GENERATING" {
		t.Fatalf("consultation was not isolated: %#v", item)
	}
	subjectBefore, _ := state.GetTask(ctx, task.ID)
	workBefore, _ := state.GetWorkDetail(ctx, task.ID)
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 1, RunID: item.LastRunID, Type: "run.completed", Output: `{"message":"任务尚未开始执行。"}`}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Consultation model.TaskConsultation `json:"consultation"`
	}
	call("GET", base, nil, 200, &response)
	if response.Consultation.State != "IDLE" || len(response.Consultation.Messages) != 2 {
		t.Fatalf("reply missing: %#v", response.Consultation)
	}
	subjectAfter, _ := state.GetTask(ctx, task.ID)
	workAfter, _ := state.GetWorkDetail(ctx, task.ID)
	if subjectAfter.State != subjectBefore.State || subjectAfter.Version != subjectBefore.Version || len(workAfter.Messages) != len(workBefore.Messages) {
		t.Fatal("consultation changed the subject task")
	}
	page := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(page, httptest.NewRequest("GET", "/assets/task-consultation.js", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "不打断、不指导工作 Agent") {
		t.Fatal("consultation UI asset missing")
	}
}
