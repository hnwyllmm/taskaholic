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

func TestWorkbenchRejectsCrossOriginWritesAndTrailingJSON(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{}, state, nil)
	for _, tc := range []struct {
		origin, body string
		status       int
	}{
		{"https://attacker.example", `{"title":"test","goal":"test"}`, 403},
		{"null", `{"title":"test","goal":"test"}`, 403},
		{"http://example.com", `{"title":"test","goal":"test"} {}`, 400},
		{"http://example.com", `{"title":"test","goal":"test"}`, 201},
	} {
		r := httptest.NewRequest("POST", "http://example.com/api/v1/work/tasks", strings.NewReader(tc.body))
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.origin, w.Code, w.Body.String())
		}
	}
}

func TestWorkbenchSchedulingAndReviewAPI(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{APIToken: "test"}, state, nil)
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
	for _, path := range []string{"/api/v1/work/tasks", "/api/v1/work/tasks?scope=roots", "/api/v1/work/tasks/missing/hierarchy", "/api/v1/work/summaries", "/api/v1/projects", "/api/v1/system"} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatal("missing authentication", path)
		}
	}
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/tasks", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "任务工作台") || !strings.Contains(w.Body.String(), "任务总结") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing UI/security headers")
	}
	var task model.Task
	call("POST", "/api/v1/work/tasks", map[string]any{"title": "Write guide", "goal": "provide guide.md"}, 201, &task)
	var roots struct {
		Tasks []model.WorkTaskItem `json:"tasks"`
	}
	call("GET", "/api/v1/work/tasks?scope=roots", nil, 200, &roots)
	if len(roots.Tasks) != 1 || roots.Tasks[0].ID != task.ID {
		t.Fatal("root work projection missing", roots)
	}
	var hierarchy model.WorkHierarchy
	call("GET", "/api/v1/work/tasks/"+task.ID+"/hierarchy", nil, 200, &hierarchy)
	if hierarchy.TaskID != task.ID || hierarchy.TaskVersion != task.Version || len(hierarchy.Children) != 0 {
		t.Fatal("invalid hierarchy", hierarchy)
	}
	call("GET", "/api/v1/work/tasks/missing/hierarchy", nil, 404, nil)
	call("GET", "/api/v1/work/tasks?scope=invalid", nil, 400, nil)
	ui := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(ui, httptest.NewRequest("GET", "/assets/task-hierarchy.js", nil))
	if ui.Code != 200 || ui.Header().Get("Content-Security-Policy") == "" || !strings.Contains(ui.Body.String(), "WATaskHierarchy") {
		t.Fatal("hierarchy UI asset unavailable")
	}
	s.scheduleWork(ctx)
	config, _ := state.GetWorkConfig(ctx, task.ID)
	if config.SchedulerError == "" {
		t.Fatal("offline task should explain waiting")
	}
	detail, _ := state.GetTaskDetail(ctx, task.ID)
	if len(detail.Runs) != 0 {
		t.Fatal("task ran without agent")
	}
	hello := model.RuntimeHello{RuntimeID: "runtime", Epoch: "epoch", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "read_only_runs": true, "structured_output": true}}}}
	if err := state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	s.hub.register(&runtimeConnection{runtimeID: hello.RuntimeID, epoch: hello.Epoch})
	draft, err := state.CreateRoleDraft(ctx, "writer", "", "")
	if err != nil {
		t.Fatal(err)
	}
	draft, err = state.UpdateRoleDraft(ctx, draft.ID, draft.Version, model.RoleSpec{Name: "writer", Capabilities: []string{"document.write"}, Instructions: "Write docs", OutputContract: "Provide full files"})
	if err != nil {
		t.Fatal(err)
	}
	role, err := state.PublishRoleDraft(ctx, draft.ID, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := state.CreateAgent(ctx, model.AgentProfile{Name: "writer-1", RoleID: role.ID, RuntimeID: hello.RuntimeID, AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleOne(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	detail, _ = state.GetTaskDetail(ctx, task.ID)
	if len(detail.Runs) != 1 || detail.Runs[0].AgentID != agent.ID {
		t.Fatal("automatic selection failed", detail.Runs)
	}
	var second model.Task
	call("POST", "/api/v1/work/tasks", map[string]any{"title": "Second task", "goal": "wait for capacity"}, 201, &second)
	if err = s.scheduleOne(ctx, second.ID); err == nil {
		t.Fatal("scheduler bypassed agent capacity")
	}
	run := detail.Runs[0]
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 1, RunID: run.ID, Type: "run.completed", Output: `{"outcome":"review","message":"请验收","artifacts":[{"name":"guide.md","content":"# Guide"}],"summary":{"result":"安装指南已经交付","learnings":["明确文件名"],"improvements":["增加自动校验"]}}`}); err != nil {
		t.Fatal(err)
	}
	wdetail, _ := state.GetWorkDetail(ctx, task.ID)
	call("POST", "/api/v1/work/tasks/"+task.ID+"/reviews/"+wdetail.Reviews[0].ID, map[string]any{"decision": "APPROVED"}, 200, nil)
	call("POST", "/api/v1/work/tasks/"+task.ID+"/reviews/"+wdetail.Reviews[0].ID, map[string]any{"decision": "APPROVED"}, 200, nil)
	call("POST", "/api/v1/work/tasks/"+task.ID+"/messages", map[string]any{"message": "late comment"}, 409, nil)
	if err = s.scheduleOne(ctx, second.ID); err != nil {
		t.Fatal("capacity did not recover", err)
	}
	var completed struct {
		Detail model.TaskDetail `json:"detail"`
	}
	call("GET", "/api/v1/work/tasks/"+task.ID, nil, 200, &completed)
	if completed.Detail.Summary == nil || completed.Detail.Summary.Result != "安装指南已经交付" || completed.Detail.Summary.Metrics.ReviewRoundCount != 1 {
		t.Fatalf("completion summary missing from task detail: %+v", completed.Detail.Summary)
	}
	var summaryList struct {
		Summaries []model.TaskSummary `json:"summaries"`
	}
	call("GET", "/api/v1/work/summaries?limit=10", nil, 200, &summaryList)
	if len(summaryList.Summaries) != 1 || summaryList.Summaries[0].TaskID != task.ID {
		t.Fatalf("summary API result: %+v", summaryList.Summaries)
	}
	call("GET", "/api/v1/work/tasks/"+second.ID+"/artifacts/"+wdetail.Artifacts[0].ID, nil, 404, nil)
	artifact := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/work/tasks/"+task.ID+"/artifacts/"+wdetail.Artifacts[0].ID, nil)
	req.Header.Set("Authorization", "Bearer test")
	s.http.Handler.ServeHTTP(artifact, req)
	if artifact.Code != 200 || artifact.Body.String() != "# Guide" || !strings.Contains(artifact.Header().Get("Content-Disposition"), "attachment") || artifact.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("artifact download unsafe/incomplete")
	}
	call("POST", "/api/v1/tasks/"+second.ID+"/runs", map[string]any{}, 409, nil)
	call("POST", "/api/v1/tasks/"+second.ID+"/subtasks", map[string]any{"title": "not yet", "goal": "no bypass"}, 409, nil)
}
