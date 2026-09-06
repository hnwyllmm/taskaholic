package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func serverReviewFixture(t *testing.T) (*Server, *store.Store, model.Task, model.Run, model.Review) {
	t.Helper()
	ctx := context.Background()
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	s := New(Config{APIToken: "test"}, state, nil)
	hello := model.RuntimeHello{RuntimeID: "review-machine", Epoch: "review-epoch", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"native_session": true, "role_instructions": true, "structured_output": true, "read_only_runs": true}}}}
	if err = state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	s.hub.register(&runtimeConnection{runtimeID: hello.RuntimeID, epoch: hello.Epoch})
	d, err := state.CreateRoleDraft(ctx, "验收演练 Agent（模拟）", "", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err = state.UpdateRoleDraft(ctx, d.ID, d.Version, model.RoleSpec{Name: "验收演练 Agent（模拟）", Capabilities: []string{"document.write"}, Instructions: "Explain evidence", OutputContract: "Document", Boundaries: []string{"No external writes"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := state.PublishRoleDraft(ctx, d.ID, d.Version)
	if err != nil {
		t.Fatal(err)
	}
	a, err := state.CreateAgent(ctx, model.AgentProfile{Name: "验收演练 Agent（模拟）", RoleID: r.ID, RuntimeID: hello.RuntimeID, AdapterID: "codex-agent", ModelID: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateWork(ctx, store.CreateWorkRequest{Title: "验收沟通演练：架构说明", Goal: "交付一份架构说明，等待沟通与验收", AgentID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleOne(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	detail, err := state.GetTaskDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := detail.Runs[0]
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 1, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:review-fixture-session"}); err != nil {
		t.Fatal(err)
	}
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 2, RunID: run.ID, Type: "run.completed", Output: `{"outcome":"review","message":"已交付架构说明，可以先和我沟通再验收。","artifacts":[{"name":"design.md","content":"# 架构说明\nManager 维护任务到 Session 的映射；Agent 负责原生记忆。验收需要明确人工确认。"}],"summary":{"result":"架构说明已交付","learnings":["Session 归属保持稳定"],"improvements":["补充验收沟通"]}}`}); err != nil {
		t.Fatal(err)
	}
	w, err := state.GetWorkDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, state, task, run, w.Reviews[0]
}

func TestReviewDiscussionAPIAndOriginalRuntimeScheduler(t *testing.T) {
	ctx := context.Background()
	s, state, task, delivery, r := serverReviewFixture(t)
	base := "/api/v1/work/tasks/" + task.ID + "/reviews/" + r.ID
	call := func(method, path string, body any, want int, target any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		out := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(out, req)
		if out.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, out.Code, out.Body.String())
		}
		if target != nil {
			if err := json.Unmarshal(out.Body.Bytes(), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, suffix := range []string{"/messages", "/messages/fake/stop"} {
		out := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(out, httptest.NewRequest("POST", base+suffix, nil))
		if out.Code != 401 {
			t.Fatal("missing auth", out.Code)
		}
	}
	x := httptest.NewRequest("POST", base+"/messages", bytes.NewBufferString(`{}`))
	x.Header.Set("Authorization", "Bearer test")
	x.Header.Set("Origin", "https://evil.invalid")
	out := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(out, x)
	if out.Code != 403 {
		t.Fatal("cross-origin accepted")
	}
	call("POST", base+"/messages", map[string]any{"message": "without key"}, 400, nil)
	var turn model.ReviewTurn
	call("POST", base+"/messages", map[string]any{"message": "为什么采用这个设计？", "idempotency_key": "q", "expected_discussion_version": 0}, 202, &turn)
	call("POST", base, map[string]any{"decision": "APPROVED", "expected_discussion_version": 1}, 409, nil)
	// Persisted presence alone is insufficient: the original live connection is required.
	s.hub = newRuntimeHub()
	if err := s.scheduleReviewTurn(ctx, turn); !errors.Is(err, model.ErrConflict) {
		t.Fatal("offline task re-routed", err)
	}
	unchanged, err := state.GetReviewTurn(ctx, turn.ID)
	if err != nil || unchanged.State != "QUEUED" || unchanged.RunID != "" {
		t.Fatal(unchanged, err)
	}
	s.hub.register(&runtimeConnection{runtimeID: "review-machine", epoch: "review-epoch"})
	if err = s.scheduleReviewTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	current, err := state.GetReviewTurn(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := state.GetRun(ctx, current.RunID)
	if err != nil || run.SessionID != delivery.SessionID || run.AgentID != delivery.AgentID {
		t.Fatal(run, err)
	}
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: "review-machine", Epoch: "review-epoch", RuntimeSeq: 3, RunID: run.ID, Type: "run.completed", Output: `{"message":"因为之前工作中验证过会话归属不应随默认配置改变。"}`}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Work   model.WorkDetail `json:"work"`
		Detail model.TaskDetail `json:"detail"`
	}
	call("GET", "/api/v1/work/tasks/"+task.ID, nil, 200, &payload)
	if len(payload.Work.ReviewTurns) != 1 || payload.Work.ReviewTurns[0].Answer == "" || payload.Detail.Task.State != model.TaskStateReview {
		t.Fatal(payload)
	}
	call("POST", base, map[string]any{"decision": "APPROVED", "expected_discussion_version": 1}, 409, nil)
	call("POST", base, map[string]any{"decision": "APPROVED", "comment": "沟通后通过", "expected_discussion_version": payload.Work.Reviews[0].DiscussionVersion}, 200, nil)
	call("POST", base+"/messages", map[string]any{"message": "late", "idempotency_key": "late", "expected_discussion_version": 3}, 409, nil)
}
