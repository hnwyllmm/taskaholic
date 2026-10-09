package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func commitTerminalPR(t *testing.T, s *Store, source model.TaskSource, target model.SourceTarget, head, kind string) {
	t.Helper()
	cursor, err := json.Marshal(map[string]any{"head": head, "ci_state": "success"})
	if err != nil {
		t.Fatal(err)
	}
	event := model.SourceEvent{
		Key:     kind + ":" + head,
		Kind:    kind,
		Entity:  target.Entity,
		HeadSHA: head,
		Message: target.Entity + "\nterminal",
	}
	if err = s.CommitSourcePoll(context.Background(), source, target, cursor, head, []model.SourceEvent{event}, 0, true); err != nil {
		t.Fatal(err)
	}
}

func TestRecordedMergedPRReconciliationCompletesObsoleteInput(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	run := startWork(t, s, agent, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:delivery"}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, run, 2, "review", "final delivery")
	if _, err := s.MessageWork(ctx, task.ID, "check the remaining gate", "gate", false); err != nil {
		t.Fatal(err)
	}
	waitingRun := startWork(t, s, agent, task)
	waitingOutput, _ := json.Marshal(workflow.Result{Outcome: "needs_input", Message: "May I edit the PR title?", Artifacts: []workflow.File{}})
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: waitingRun.ID, TaskID: task.ID, Type: "run.completed", Output: string(waitingOutput)}); err != nil {
		t.Fatal(err)
	}
	waiting, _ := s.GetTask(ctx, task.ID)
	if waiting.State != model.TaskStateInput {
		t.Fatal("task did not reach input state", waiting.State)
	}

	source := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/1397")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	commitTerminalPR(t, s, source, target, head, "github.merged")
	// Simulate an event already consumed by the pre-fix release. No PENDING
	// inbox work remains, so only durable reconciliation can repair the task.
	if _, err = s.db.Exec(`UPDATE source_event SET state='RECORDED',data_json=json_set(data_json,'$.state','RECORDED') WHERE target_id=?`, target.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}

	done, _ := s.GetTask(ctx, task.ID)
	if done.State != model.TaskStateCompleted {
		t.Fatal("merged delivery did not complete task", done.State)
	}
	detail, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range detail.Messages {
		if message.Delivery == "PENDING" {
			t.Fatal("obsolete input survived merge", message)
		}
	}
	summary, err := s.GetLatestTaskSummary(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Result != "Delivered final delivery" || summary.SourceType != "run" || summary.SourceID != run.ID {
		t.Fatalf("merge summary did not use the last delivery result: %+v", summary)
	}
	var completions int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='TaskAutoCompletedByMergedPRs'`, task.ID).Scan(&completions); err != nil || completions != 1 {
		t.Fatal("completion audit event missing", completions, err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM task_summary WHERE task_id=?`, task.ID).Scan(&completions); err != nil || completions != 1 {
		t.Fatal("reconciliation was not idempotent", completions, err)
	}
}

func TestAllDeliveryPRsMustBeMergedAndClosedUnmergedIsNotSuccess(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	run := startWork(t, s, agent, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:multiple"}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, run, 2, "review", "multiple PRs")
	source := saveTestSource(t, s, "github")
	first, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/1401")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/1402")
	if err != nil {
		t.Fatal(err)
	}
	commitTerminalPR(t, s, source, first, strings.Repeat("b", 40), "github.merged")
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if current.State == model.TaskStateCompleted {
		t.Fatal("one of two PRs completed the task")
	}
	commitTerminalPR(t, s, source, second, strings.Repeat("c", 40), "github.closed")
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetTask(ctx, task.ID)
	if current.State == model.TaskStateCompleted {
		t.Fatal("closed-unmerged PR was treated as successful delivery")
	}
	if _, err = s.GetLatestTaskSummary(ctx, task.ID); err != sql.ErrNoRows {
		t.Fatal("unsuccessful terminal delivery created summary", err)
	}
}

func TestMergedPRStopsObsoleteReviewerWork(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	deliveryRun := startWork(t, s, agent, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: deliveryRun.ID, SessionID: deliveryRun.SessionID, Type: "session.bound", AgentSessionRef: "codex:review-cleanup"}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, deliveryRun, 2, "review", "delivery before reviewer cleanup")
	source := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/1403")
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateWork(ctx, CreateWorkRequest{
		Title:        "Review pull request",
		Goal:         "Review the fixed commit",
		Key:          "review-before-merge",
		Source:       "router.review",
		Requirements: model.TaskRequirements{RoleID: agent.RoleID},
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewRun := startWork(t, s, agent, child)
	head := strings.Repeat("d", 40)
	if err = s.sourceWrite(ctx, func(tx *sql.Tx) error {
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO source_review(target_id,head_sha,role_id,task_id,state) VALUES(?,?,?,?,'PENDING')`, target.ID, head, agent.RoleID, child.ID)
		return insertErr
	}); err != nil {
		t.Fatal(err)
	}
	commitTerminalPR(t, s, source, target, head, "github.merged")
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}

	parent, _ := s.GetTask(ctx, task.ID)
	reviewer, _ := s.GetTask(ctx, child.ID)
	if parent.State != model.TaskStateCompleted || reviewer.State != model.TaskStatePaused {
		t.Fatal("merge did not terminate parent and pause obsolete reviewer", parent.State, reviewer.State)
	}
	var reviewState string
	if err = s.db.QueryRow(`SELECT state FROM source_review WHERE task_id=?`, child.ID).Scan(&reviewState); err != nil || reviewState != "SUPERSEDED" {
		t.Fatal("review was not superseded", reviewState, err)
	}
	var directives int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM directive WHERE run_id=? AND kind=? AND state=?`, reviewRun.ID, model.DirectiveKindInterrupt, model.DirectiveStateQueued).Scan(&directives); err != nil || directives != 1 {
		t.Fatal("obsolete reviewer was not interrupted exactly once", directives, err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM directive WHERE run_id=? AND kind=? AND state=?`, reviewRun.ID, model.DirectiveKindInterrupt, model.DirectiveStateQueued).Scan(&directives); err != nil || directives != 1 {
		t.Fatal("merge reconciliation duplicated reviewer interruption", directives, err)
	}
}
