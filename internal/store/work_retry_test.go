package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func TestWorkRetryPreservesApprovalAndSession(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	first := startWork(t, s, dev, task)
	developmentFinish(t, s, first, 1, submittedPlan("plan"))
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	rr := startWork(t, s, reviewer, child)
	developmentFinish(t, s, rr, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "passed", Artifacts: []workflow.File{}})
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", "yes"); err != nil {
		t.Fatal(err)
	}
	impl := startWork(t, s, dev, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: impl.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: impl.ID, TaskID: task.ID, Type: "run.failed", Error: "git failed"}); err != nil {
		t.Fatal(err)
	}
	before := developmentState(t, s, task.ID)
	current, _ := s.GetTask(ctx, task.ID)
	if _, err := s.RetryWork(ctx, task.ID, current.Version-1); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale retry accepted", err)
	}
	if _, err := s.RetryWork(ctx, task.ID, current.Version); err != nil {
		t.Fatal(err)
	}
	after := developmentState(t, s, task.ID)
	if after.Phase != "IMPLEMENTING" || after.ApprovedReviewID != before.ApprovedReviewID || after.PlanHash != before.PlanHash || after.Version != before.Version {
		t.Fatal("retry changed approval", after)
	}
	if _, err := s.RetryWork(ctx, task.ID, current.Version); !errors.Is(err, model.ErrConflict) {
		t.Fatal("duplicate retry accepted", err)
	}
	next := startWork(t, s, dev, task)
	if next.SessionID != first.SessionID || outboxSpec(t, s, next.ID).ExecutionGrant == nil {
		t.Fatal("retry lost session/grant")
	}
}

func TestRetryWorkPlanningAndStateGuards(t *testing.T) {
	ctx := context.Background()
	s, agent, _, task := developmentFixture(t)
	run := startWork(t, s, agent, task)
	current, _ := s.GetTask(ctx, task.ID)
	if _, err := s.RetryWork(ctx, task.ID, current.Version); !errors.Is(err, model.ErrConflict) {
		t.Fatal("active retry accepted", err)
	}
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.failed", Error: "You've hit your usage limit"}); err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetTask(ctx, task.ID)
	if _, err := s.RetryWork(ctx, task.ID, current.Version); err != nil {
		t.Fatal(err)
	}
	if d := developmentState(t, s, task.ID); d.Phase != "PLANNING" {
		t.Fatal(d)
	}
	next := startWork(t, s, agent, task)
	if next.SessionID != run.SessionID {
		t.Fatal("session changed")
	}
	developmentFinish(t, s, next, 2, submittedPlan("plan"))
	current, _ = s.GetTask(ctx, task.ID)
	if _, err := s.RetryWork(ctx, task.ID, current.Version); !errors.Is(err, model.ErrConflict) {
		t.Fatal("review bypass", err)
	}
}

func TestRetryWorkContinuesInterruptedApprovedDevelopment(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	planRun := startWork(t, s, dev, task)
	developmentFinish(t, s, planRun, 1, submittedPlan("plan"))
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	reviewerRun := startWork(t, s, reviewer, child)
	developmentFinish(t, s, reviewerRun, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "passed", Artifacts: []workflow.File{}})
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", "yes"); err != nil {
		t.Fatal(err)
	}
	implementation := startWork(t, s, dev, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: implementation.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: implementation.ID, TaskID: task.ID, Type: "run.interrupted", Error: "context canceled"}); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if current.State != model.TaskStatePaused {
		t.Fatal("interrupted task is not paused", current.State)
	}
	before := developmentState(t, s, task.ID)
	if _, err := s.RetryWork(ctx, task.ID, current.Version); err != nil {
		t.Fatal(err)
	}
	after := developmentState(t, s, task.ID)
	if after.Phase != "IMPLEMENTING" || after.ApprovedReviewID != before.ApprovedReviewID || after.PlanHash != before.PlanHash || after.Version != before.Version {
		t.Fatal("continue changed approved development", after)
	}
	next := startWork(t, s, dev, task)
	if next.SessionID != implementation.SessionID || outboxSpec(t, s, next.ID).ExecutionGrant == nil {
		t.Fatal("continue lost session or execution grant")
	}
}

func TestRetryWorkResumesExplicitPauseAfterCompletedRun(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	run := startWork(t, s, agent, task)
	result := `{"outcome":"needs_input","message":"waiting before pause","artifacts":[]}`
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.completed", Output: result}); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	paused, _ := s.GetTask(ctx, task.ID)
	if paused.State != model.TaskStatePaused {
		t.Fatal("task was not explicitly paused", paused.State)
	}
	message, err := s.RetryWork(ctx, task.ID, paused.Version)
	if err != nil {
		t.Fatal(err)
	}
	resumed, _ := s.GetTask(ctx, task.ID)
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if resumed.State != model.TaskStateQueued || work.Config.Paused || message.Delivery != "PENDING" || !strings.Contains(message.Content, "恢复了此前明确暂停") {
		t.Fatalf("explicit pause was not resumed: task=%+v work=%+v message=%+v", resumed, work.Config, message)
	}
	next := startWork(t, s, agent, task)
	if next.SessionID != run.SessionID {
		t.Fatal("resume lost session affinity", next.SessionID, run.SessionID)
	}
}

func TestRetryWorkResumesExplicitPauseBeforeFirstRun(t *testing.T) {
	ctx := context.Background()
	s, _, task := workFixture(t)
	if err := s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	paused, _ := s.GetTask(ctx, task.ID)
	if _, err := s.RetryWork(ctx, task.ID, paused.Version); err != nil {
		t.Fatal(err)
	}
	resumed, _ := s.GetTask(ctx, task.ID)
	if resumed.State != model.TaskStateQueued {
		t.Fatal("pre-run pause was not resumed", resumed.State)
	}
}

func TestRetryWorkReprocessesCompletedOutputAfterParserUpgrade(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	run := startWork(t, s, agent, task)
	invalid := `{"outcome":"review","message":"QA complete","artifacts":[{"name":"empty.md","content":""}],"summary":{"result":"reviewed","learnings":[],"improvements":[]}}`
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.completed", Output: invalid}); err != nil {
		t.Fatal(err)
	}
	validAfterUpgrade := `{"outcome":"review","message":"QA complete","artifacts":[{"name":"qa.md","content":"findings"},{"name":"memory-citation.txt","content":""}],"summary":{"result":"reviewed","learnings":[],"improvements":[]}}`
	if _, err := s.db.ExecContext(ctx, `UPDATE run SET output=? WHERE run_id=?`, validAfterUpgrade, run.ID); err != nil {
		t.Fatal(err)
	}
	// Later source/reviewer activity may clear the transient scheduler error.
	// The durable rejection receipt must still make this completed run retryable.
	if _, err := s.db.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error='' WHERE task_id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	message, err := s.RetryWork(ctx, task.ID, current.Version)
	if err != nil {
		t.Fatal(err)
	}
	if message.Delivery != "RECORDED" || message.RunID != run.ID {
		t.Fatalf("missing replay receipt: %+v", message)
	}
	current, _ = s.GetTask(ctx, task.ID)
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if current.State != model.TaskStateReview || work.Config.Paused || work.Config.SchedulerError != "" || len(work.Artifacts) != 1 || work.Artifacts[0].Name != "qa.md" || len(work.Reviews) != 1 {
		t.Fatalf("completed output was not reprocessed: task=%+v work=%+v", current, work)
	}
	if len(work.Messages) != 4 || work.Messages[2].Speaker != "assistant" || work.Messages[3].Speaker != "system" {
		t.Fatalf("unexpected replay messages: %+v", work.Messages)
	}
}

func TestRetryWorkQueuesCorrectionForStillInvalidCompletedOutput(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	run := startWork(t, s, agent, task)
	invalid := `{"outcome":"review","message":"done","artifacts":[{"name":"empty.md","content":""}]}`
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.completed", Output: invalid}); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	message, err := s.RetryWork(ctx, task.ID, current.Version)
	if err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetTask(ctx, task.ID)
	if current.State != model.TaskStateQueued || message.Delivery != "PENDING" || !strings.Contains(message.Content, "仅重新提交") {
		t.Fatalf("invalid result correction was not queued: task=%+v message=%+v", current, message)
	}
}

func TestRetryWorkQueuesCorrectionWhenRejectedOutputWasAlreadyMaterialized(t *testing.T) {
	ctx := context.Background()
	s, developer, _, task := developmentFixture(t)
	run := startWork(t, s, developer, task)
	plan := submittedPlan("preserve this plan artifact")
	plan.PullRequests = []workflow.PullRequest{{URL: "https://github.com/oceanbase/seekdb/pull/9999"}}
	developmentFinish(t, s, run, 1, plan)
	current, _ := s.GetTask(ctx, task.ID)
	if current.State != model.TaskStateBlocked {
		t.Fatal("fixture did not produce a manager block", current.State)
	}
	message, err := s.RetryWork(ctx, task.ID, current.Version)
	if err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetTask(ctx, task.ID)
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if current.State != model.TaskStateQueued || message.Delivery != "PENDING" || len(work.Artifacts) != 1 || !strings.Contains(message.Content, "上轮消息和产物已保留") {
		t.Fatalf("materialized rejection was not safely continued: task=%+v message=%+v artifacts=%+v", current, message, work.Artifacts)
	}
}
