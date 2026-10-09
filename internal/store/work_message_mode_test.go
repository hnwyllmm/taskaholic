package store

import (
	"context"
	"errors"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func approvedImplementation(t *testing.T) (*Store, model.AgentProfile, model.Task, model.Run) {
	t.Helper()
	ctx := context.Background()
	s, developer, reviewer, task := developmentFixture(t)
	planRun := startWork(t, s, developer, task)
	developmentFinish(t, s, planRun, 1, submittedPlan("approved plan"))
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	child, err := s.GetTask(ctx, d.ReviewerTaskID)
	if err != nil {
		t.Fatal(err)
	}
	reviewerRun := startWork(t, s, reviewer, child)
	developmentFinish(t, s, reviewerRun, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "passed", Artifacts: []workflow.File{}})
	work, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "PLAN_APPROVED", "approved"); err != nil {
		t.Fatal(err)
	}
	return s, developer, task, startWork(t, s, developer, task)
}

func TestExecutionDirectionPreservesApprovedPlanAndGrant(t *testing.T) {
	ctx := context.Background()
	s, developer, task, implementation := approvedImplementation(t)
	before := *developmentState(t, s, task.ID)

	if _, err := s.MessageWorkWithMode(ctx, task.ID, "stop optional tests and prepare the PR", "direction", true, WorkMessageExecutionDirection); err != nil {
		t.Fatal(err)
	}
	after := developmentState(t, s, task.ID)
	if after.Phase != "IMPLEMENTING" || after.Version != before.Version || after.PlanHash != before.PlanHash || after.ApprovedReviewID != before.ApprovedReviewID {
		t.Fatal("execution direction invalidated approved development", after)
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='ExecutionDirectionQueued'`, task.ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("execution direction audit event missing", events, err)
	}
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: implementation.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: implementation.ID, TaskID: task.ID, Type: "run.interrupted", Error: "context canceled"}); err != nil {
		t.Fatal(err)
	}
	next := startWork(t, s, developer, task)
	spec := outboxSpec(t, s, next.ID)
	if next.SessionID != implementation.SessionID || spec.ExecutionGrant == nil || spec.ExecutionGrant.PlanHash != before.PlanHash || spec.ExecutionGrant.ReviewID != before.ApprovedReviewID {
		t.Fatal("execution direction lost session or grant", next, spec.ExecutionGrant)
	}
}

func TestExecutionDirectionRequiresApprovedImplementation(t *testing.T) {
	ctx := context.Background()
	s, _, _, task := developmentFixture(t)
	if _, err := s.MessageWorkWithMode(ctx, task.ID, "skip review", "direction", false, WorkMessageExecutionDirection); !errors.Is(err, model.ErrConflict) {
		t.Fatal("execution direction bypassed plan approval", err)
	}
	if _, err := s.MessageWorkWithMode(ctx, task.ID, "invalid", "invalid", false, "other"); !errors.Is(err, model.ErrValidation) {
		t.Fatal("invalid work message mode accepted", err)
	}
}
