package store

import (
	"context"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func TestLateVMResultResumesOriginalApprovedDevelopmentSession(t *testing.T) {
	ctx := context.Background()
	s, dev, task, implementation := approvedEnvironmentFixture(t)
	before := developmentState(t, s, task.ID)
	operationID := strings.Repeat("a", 32)
	developmentFinish(t, s, implementation, 3, workflow.Result{
		Outcome: "blocked", Message: "Windows operation " + operationID + " has not returned yet.", Artifacts: []workflow.File{},
	})
	blocked, _ := s.GetTask(ctx, task.ID)
	if blocked.State != model.TaskStateBlocked {
		t.Fatal("fixture did not block", blocked.State)
	}
	duplicate, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: implementation.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 4, RunID: implementation.ID, TaskID: task.ID,
		Type: "vm.operation.completed", Attributes: map[string]any{"operation_id": operationID, "operation": "exec", "exit_code": float64(1), "result_available": true},
	})
	if err != nil || duplicate {
		t.Fatal("late result rejected", duplicate, err)
	}
	queued, _ := s.GetTask(ctx, task.ID)
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if queued.State != model.TaskStateQueued || work.Config.Paused || work.Config.SchedulerError != "" {
		t.Fatal("late result did not queue continuation", queued.State, work.Config)
	}
	var continuation *model.TaskMessage
	for index := range work.Messages {
		if work.Messages[index].Delivery == "PENDING" {
			continuation = &work.Messages[index]
		}
	}
	if continuation == nil || !strings.Contains(continuation.Content, "result "+operationID) || !strings.Contains(continuation.Content, "不会重跑") {
		t.Fatal("missing safe result instruction", work.Messages)
	}
	after := developmentState(t, s, task.ID)
	if after.Phase != "IMPLEMENTING" || after.PlanHash != before.PlanHash || after.ApprovedReviewID != before.ApprovedReviewID || after.Version != before.Version {
		t.Fatal("late result changed approved plan", after)
	}
	next := startWork(t, s, dev, task)
	if next.SessionID != implementation.SessionID || outboxSpec(t, s, next.ID).ExecutionGrant == nil {
		t.Fatal("late result lost original session or grant")
	}
}

func TestLateVMResultDoesNotResumeUnrelatedBlock(t *testing.T) {
	ctx := context.Background()
	s, _, task, implementation := approvedEnvironmentFixture(t)
	operationID := strings.Repeat("b", 32)
	developmentFinish(t, s, implementation, 3, workflow.Result{
		Outcome: "blocked", Message: "A different business dependency is unavailable.", Artifacts: []workflow.File{},
	})
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: implementation.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 4, RunID: implementation.ID, TaskID: task.ID,
		Type: "vm.operation.completed", Attributes: map[string]any{"operation_id": operationID, "operation": "exec", "exit_code": 0, "result_available": true},
	}); err != nil {
		t.Fatal(err)
	}
	blocked, _ := s.GetTask(ctx, task.ID)
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if blocked.State != model.TaskStateBlocked {
		t.Fatal("unrelated late result resumed task", blocked.State)
	}
	for _, message := range work.Messages {
		if message.Delivery == "PENDING" {
			t.Fatal("unrelated late result queued work", message)
		}
	}
}
