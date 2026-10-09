package store

import (
	"context"
	"errors"
	"reflect"
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

func TestGuidanceCannotAuthorizePlanning(t *testing.T) {
	ctx := context.Background()
	s, developer, _, task := developmentFixture(t)
	startWork(t, s, developer, task)
	if _, err := s.MessageWorkWithMode(ctx, task.ID, "narrow the tests", "direction", false, WorkMessageExecutionDirection); err != nil {
		t.Fatal(err)
	}
	if d := developmentState(t, s, task.ID); d.Phase != "PLANNING" || d.ApprovedReviewID != "" {
		t.Fatal("guidance bypassed plan approval", d)
	}
	if _, err := s.MessageWorkWithMode(ctx, task.ID, "invalid", "invalid", false, "other"); !errors.Is(err, model.ErrValidation) {
		t.Fatal("invalid work message mode accepted", err)
	}
}

func TestDefaultGuidanceAndExplicitReturn(t *testing.T) {
	for _, mode := range []string{"", "plan_change", WorkMessageExecutionDirection} {
		t.Run("default-"+mode, func(t *testing.T) {
			s, _, task, _ := approvedImplementation(t)
			before := *developmentState(t, s, task.ID)
			if _, err := s.MessageWorkWithMode(context.Background(), task.ID, "change test scope", "guidance", false, mode); err != nil {
				t.Fatal(err)
			}
			after := developmentState(t, s, task.ID)
			if !reflect.DeepEqual(*after, before) {
				t.Fatal("default guidance changed development", after)
			}
		})
	}
	for mode, target := range map[string]string{WorkMessagePlanChange: "PLANNING", WorkMessageAgentReview: "AGENT_REVIEW"} {
		t.Run(mode, func(t *testing.T) {
			s, _, task, _ := approvedImplementation(t)
			before := *developmentState(t, s, task.ID)
			if _, err := s.MessageWorkWithMode(context.Background(), task.ID, "explicitly return", "return", false, mode); err != nil {
				t.Fatal(err)
			}
			after := developmentState(t, s, task.ID)
			if after.Phase != target || after.Version != before.Version+1 || after.ApprovedReviewID != "" {
				t.Fatal("explicit return was not applied", after)
			}
		})
	}
}

func TestAgentReplanRequestsHumanDecisionWithoutRollback(t *testing.T) {
	ctx := context.Background()
	s, _, task, run := approvedImplementation(t)
	before := *developmentState(t, s, task.ID)
	result := submittedPlan("public interface change proposed")
	result.Outcome = "replan"
	result.PlanChange = &workflow.PlanChange{Kind: "external_contract", ApprovedAssumption: "The public interface stays unchanged.", NewEvidence: "The requested behavior requires a different public interface.", AffectedAreas: []string{"public interface"}}
	result.TaskUpdate = &workflow.TaskUpdate{Kind: "feature", Analysis: "The interface is insufficient.", Approach: "Propose a new interface.", Reason: "New evidence contradicts the assumption.", Validation: "A compatibility test is required."}
	developmentFinish(t, s, run, 3, result)
	after := developmentState(t, s, task.ID)
	current, _ := s.GetTask(ctx, task.ID)
	if !reflect.DeepEqual(*after, before) || current.State != model.TaskStateInput {
		t.Fatal("Agent rolled back rather than requesting a decision", after, current)
	}
}

func TestHumanReviewGuidanceStaysInHumanReview(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	first := startWork(t, s, dev, task)
	developmentFinish(t, s, first, 1, submittedPlan("first plan"))
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	reviewRun := startWork(t, s, reviewer, child)
	developmentFinish(t, s, reviewRun, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "passed", Artifacts: []workflow.File{}})
	if _, err := s.MessageWork(ctx, task.ID, "reduce optional testing", "guidance", false); err != nil {
		t.Fatal(err)
	}
	if d := developmentState(t, s, task.ID); d.Phase != "HUMAN_REVIEW" {
		t.Fatal("human feedback rolled back", d)
	}
	second := startWork(t, s, dev, task)
	if !outboxSpec(t, s, second.ID).ReadOnly || second.SessionID != first.SessionID {
		t.Fatal("revision lost session or gained write access")
	}
	developmentFinish(t, s, second, 3, submittedPlan("revised plan"))
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if w.Development.Phase != "HUMAN_REVIEW" || len(w.Reviews) != 2 || w.Reviews[0].State != "PENDING" || w.Reviews[0].RunID != second.ID {
		t.Fatal("revision did not continue human review", w.Development, w.Reviews)
	}
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	if child, _ := s.GetTask(ctx, child.ID); child.State != model.TaskStateCompleted {
		t.Fatal("human revision restarted Agent review", child)
	}
	if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "CHANGES_REQUESTED", "clarify one more detail"); err != nil {
		t.Fatal(err)
	}
	if d := developmentState(t, s, task.ID); d.Phase != "HUMAN_REVIEW" {
		t.Fatal("changes requested button rolled back", d)
	}
	third := startWork(t, s, dev, task)
	developmentFinish(t, s, third, 4, submittedPlan("final human revision"))
	w, _ = s.GetWorkDetail(ctx, task.ID)
	if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", "approved"); err != nil {
		t.Fatal(err)
	}
	if d := developmentState(t, s, task.ID); d.Phase != "IMPLEMENTING" {
		t.Fatal("revised plan could not be approved", d)
	}
}
