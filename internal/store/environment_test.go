package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func approvedEnvironmentFixture(t *testing.T) (*Store, model.AgentProfile, model.Task, model.Run) {
	t.Helper()
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	first := startWork(t, s, dev, task)
	developmentFinish(t, s, first, 1, submittedPlan("Windows Phase 0"))
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
	return s, dev, task, startWork(t, s, dev, task)
}

func TestEnvironmentChildReturnsToOriginalApprovedSession(t *testing.T) {
	ctx := context.Background()
	s, dev, task, impl := approvedEnvironmentFixture(t)
	approval := developmentState(t, s, task.ID)
	developmentFinish(t, s, impl, 3, workflow.Result{Outcome: "blocked", Message: "need Windows", Artifacts: []workflow.File{}, EnvironmentRequest: &workflow.EnvironmentRequest{Profile: "windows_seekdb_phase0", Reason: "Phase 0 policy matrix"}})
	parent, _ := s.GetTask(ctx, task.ID)
	if parent.State != model.TaskStateWaiting {
		t.Fatal(parent.State)
	}
	var childID string
	if err := s.db.QueryRow(`SELECT task_id FROM environment_job WHERE parent_task_id=?`, task.ID).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	child, _ := s.GetTask(ctx, childID)
	run := startWork(t, s, dev, child)
	spec := outboxSpec(t, s, run.ID)
	if spec.Environment == nil || spec.Environment.SourceSessionID != impl.SessionID || !spec.ReadOnly || spec.ExecutionGrant != nil {
		t.Fatalf("bad executor grant: %+v", spec)
	}
	developmentFinish(t, s, run, 4, workflow.Result{Outcome: "review", Message: "Windows report", Artifacts: []workflow.File{{Name: "windows.json", Content: "report"}}, EnvironmentResult: &model.EnvironmentResult{Profile: "windows_seekdb_phase0", Status: "failed", Message: "probe compile failed"}})
	parent, _ = s.GetTask(ctx, task.ID)
	if parent.State != model.TaskStateQueued {
		t.Fatal("parent not resumed", parent.State)
	}
	w, _ := s.GetWorkDetail(ctx, childID)
	completed, _ := s.GetTask(ctx, childID)
	if completed.State != model.TaskStateCompleted || len(w.Reviews) != 0 || len(w.Artifacts) != 1 {
		t.Fatal("child should report without human gate")
	}
	next := startWork(t, s, dev, task)
	after := developmentState(t, s, task.ID)
	if next.SessionID != impl.SessionID || after.PlanHash != approval.PlanHash || after.ApprovedReviewID != approval.ApprovedReviewID || after.Phase != "IMPLEMENTING" {
		t.Fatal("lost session/approval")
	}
	backup := filepath.Join(t.TempDir(), "copy.sqlite")
	if err := s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var verdict string
	if err = restored.db.QueryRow(`SELECT state FROM environment_job WHERE task_id=?`, childID).Scan(&verdict); err != nil || verdict != "failed" {
		t.Fatal("lost execution verdict", err, verdict)
	}
}

func TestEnvironmentExplicitBlockedRequestAndPausedParent(t *testing.T) {
	ctx := context.Background()
	s, dev, task, impl := approvedEnvironmentFixture(t)
	developmentFinish(t, s, impl, 3, workflow.Result{Outcome: "blocked", Message: "sandbox cannot WinRM", Artifacts: []workflow.File{}})
	current, _ := s.GetTask(ctx, task.ID)
	request := workflow.EnvironmentRequest{Profile: "windows_seekdb_phase0", Reason: "approved controlled validation"}
	if err := s.RequestEnvironment(ctx, task.ID, current.Version-1, request); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale request accepted", err)
	}
	if err := s.RequestEnvironment(ctx, task.ID, current.Version, request); err != nil {
		t.Fatal(err)
	}
	var childID string
	s.db.QueryRow(`SELECT task_id FROM environment_job WHERE parent_task_id=?`, task.ID).Scan(&childID)
	child, _ := s.GetTask(ctx, childID)
	run := startWork(t, s, dev, child)
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return pauseWorkTx(ctx, tx, task.ID) }); err != nil {
		t.Fatal(err)
	}
	developmentFinish(t, s, run, 4, workflow.Result{Outcome: "review", Message: "report", Artifacts: []workflow.File{}, EnvironmentResult: &model.EnvironmentResult{Profile: "wrong", Status: "passed"}})
	current, _ = s.GetTask(ctx, task.ID)
	if current.State != model.TaskStatePaused {
		t.Fatal("paused parent resumed", current.State)
	}
	var state string
	s.db.QueryRow(`SELECT state FROM environment_job WHERE task_id=?`, childID).Scan(&state)
	if state != "unavailable" {
		t.Fatal("bad profile trusted", state)
	}
}
