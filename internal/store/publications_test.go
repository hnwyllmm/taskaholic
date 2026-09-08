package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func TestPublicationBackupAndSchemaUpgradePreserveSessions(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _, child, run, _ := qaTestFixture(t)
	reviewSubmission(t, s, run, 3, "passed")
	before, err := s.GetTaskSession(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Rehearse v15 -> v16 against a populated snapshot, without projection/writes.
	oldPath := filepath.Join(t.TempDir(), "v15.sqlite")
	if err = s.Backup(ctx, oldPath); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE permission_request`, `DROP TABLE execution_policy`, `DROP TABLE environment_job`, `DROP TABLE development_run`, `DROP TABLE development`, `DROP TABLE publication_history`, `DROP TABLE publication`, `DELETE FROM schema_version WHERE version>=16`} {
		if _, err = old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	upgraded, err := Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	after, err := upgraded.GetTaskSession(ctx, child.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed session", err)
	}
	list, err := upgraded.ListPublications(ctx, parent.ID)
	if err != nil || len(list) != 0 {
		t.Fatal("migration initiated writes", list, err)
	}
	if err = s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListPublications(ctx, parent.ID)
	p, err := s.ClaimPublication(ctx, list[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err = s.Backup(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.ListPublications(ctx, parent.ID)
	if err != nil || len(got) != 1 || got[0].State != "SUBMITTING" || got[0].Body != p.Body {
		t.Fatal("lost submission across restart", got, err)
	}
	// Expire only the test fixture's crash lease; a recovered claim must reconcile.
	if _, err = restored.db.Exec(`UPDATE publication SET next_attempt_ms=0,data_json=json_set(data_json,'$.next_attempt_ms',0)`); err != nil {
		t.Fatal(err)
	}
	recovered, err := restored.ClaimPublication(ctx, p.Key)
	if err != nil || recovered.State != "UNCERTAIN" {
		t.Fatal("crash allowed fresh POST", recovered, err)
	}
}

func reviewSubmission(t *testing.T, s *Store, run model.Run, seq int64, decision string) {
	t.Helper()
	result := workflow.Result{Outcome: "review", ReviewDecision: decision, Message: "Check foo.go:10, persistence behavior verified", Artifacts: []workflow.File{}}
	raw, _ := json.Marshal(result)
	if _, err := s.ApplyRuntimeEvent(context.Background(), model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: seq, RunID: run.ID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
}
func TestReviewPublicationStickyAcrossHeadsAndAppendOnlyHistory(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _, child, run, target := qaTestFixture(t)
	reviewSubmission(t, s, run, 3, "passed")
	if err := s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	publications, err := s.ListPublications(ctx, parent.ID)
	if err != nil || len(publications) != 1 {
		t.Fatal(publications, err)
	}
	p := publications[0]
	if p.TaskID != child.ID || p.HeadSHA != target.HeadSHA || p.Verdict != "passed" || !strings.Contains(p.Body, "**通过**") {
		t.Fatal(p)
	}
	if err = s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	same, _ := s.ListPublications(ctx, child.ID)
	if same[0].Version != p.Version {
		t.Fatal("unchanged projection created version")
	}
	claimed, err := s.ClaimPublication(ctx, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPublication(ctx, claimed, "101", p.URL+"#issuecomment-101", "SYNCED", ""); err != nil {
		t.Fatal(err)
	}
	source, err := s.GetTaskSource(ctx, target.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	pollStore(t, s, source, target, head, model.SourceEvent{Key: "head:" + head, Kind: "github.head", HeadSHA: head, Message: "new commit"})
	if err = s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	next, _ := s.ListPublications(ctx, parent.ID)
	if len(next) != 1 || next[0].Key != p.Key || next[0].RemoteID != "101" || next[0].HeadSHA != head || next[0].Verdict == "passed" || next[0].TaskID == child.ID {
		t.Fatal(next)
	}
	var history int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM publication_history`).Scan(&history); err != nil || history < 4 {
		t.Fatal(history, err)
	}
}

func TestReviewCommentCannotInheritRequiredTestsFromOlderCommit(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _, _, first, target := qaTestFixture(t)
	testSubmission(t, s, first, 3, initialTestRequest(target))
	source, err := s.GetTaskSource(ctx, target.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	pollStore(t, s, source, target, head, model.SourceEvent{Key: "head:" + head, Kind: "github.head", HeadSHA: head, Message: "new commit"})
	reviews, err := s.ListSourceReviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var current model.Task
	for _, r := range reviews {
		if r.HeadSHA == head {
			current, err = s.GetTask(ctx, r.TaskID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	qa, err := s.GetAgent(ctx, first.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, qa, current)
	reviewSubmission(t, s, run, 4, "passed")
	if err = s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListPublications(ctx, parent.ID)
	if err != nil || len(all) != 1 || all[0].Verdict == "passed" || !strings.Contains(all[0].Body, "必需测试尚未成功") {
		t.Fatal(all, err)
	}
}
func TestPublicationAmbiguityKeepsAttemptedBodyAndBlocksPrematureAcceptance(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _, _, run, _ := qaTestFixture(t)
	reviewSubmission(t, s, run, 3, "passed")
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return guardReviewPublicationsTx(ctx, tx, parent.ID) }); !errors.Is(err, model.ErrConflict) {
		t.Fatal("unpublished accepted", err)
	}
	if err := s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListPublications(ctx, parent.ID)
	p, err := s.ClaimPublication(ctx, all[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	replacement := p
	replacement.Body = "different desired report"
	if err = s.sourceWrite(ctx, func(tx *sql.Tx) error { return queuePublicationTx(ctx, tx, replacement) }); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ListPublications(ctx, parent.ID)
	if all[0].Body != p.Body || all[0].Version != p.Version {
		t.Fatal("lost attempted body")
	}
	if err = s.FinishPublication(ctx, p, "101", p.URL+"#issuecomment-101", "SYNCED", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.sourceWrite(ctx, func(tx *sql.Tx) error { return guardReviewPublicationsTx(ctx, tx, parent.ID) }); err != nil {
		t.Fatal(err)
	}
}
func TestReviewerContinuationRetainsSessionAndHonorsHold(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, child, run, target := qaTestFixture(t)
	reviewSubmission(t, s, run, 3, "changes_requested")
	before, err := s.GetTaskSession(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	resume := func(key string) error {
		return s.sourceWrite(ctx, func(tx *sql.Tx) error { return continuePRReviewsTx(ctx, tx, target, key, "author replied") })
	}
	if err = resume("reply-1"); err != nil {
		t.Fatal(err)
	}
	if err = resume("reply-1"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetTaskSession(ctx, child.ID)
	if before.ID != after.ID || before.AgentID != after.AgentID {
		t.Fatal("session changed")
	}
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM task_message WHERE task_id=? AND delivery='PENDING'`, child.ID).Scan(&count)
	if count != 1 {
		t.Fatal("duplicate continuation", count)
	}
	if _, err = s.db.Exec(`UPDATE task_workflow SET paused=1 WHERE task_id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	if err = resume("reply-2"); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM task_message WHERE task_id=? AND delivery='PENDING'`, child.ID).Scan(&count)
	if count != 1 {
		t.Fatal("manual hold ignored")
	}
}
func TestOriginalIssueStructuredWritebackAndBindingGuards(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	source := saveTestSource(t, s, "github")
	url := "https://github.com/oceanbase/seekdb/issues/123"
	if err := s.BindGitHubIssue(ctx, task.ID, source.ID, url); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	result := workflow.Result{Outcome: "blocked", Message: "cannot reproduce", Artifacts: []workflow.File{}, TaskUpdate: &workflow.TaskUpdate{Kind: "bug", Analysis: "root cause not established", Approach: "isolated reproduction", Reason: "avoid speculative fixes", Validation: "reproducer unavailable", BlockedReason: "missing affected version"}}
	raw, _ := json.Marshal(result)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := s.ListPublications(ctx, task.ID)
	if err != nil || len(p) != 0 {
		t.Fatal(p, err)
	}
	for _, state := range []string{"RUNNING", "BLOCKED", "WAITING_ENVIRONMENT", "WAITING_AUTHORIZATION", "WAITING_REVIEW"} {
		if _, err := s.db.Exec(`UPDATE task SET state=? WHERE task_id=?`, state, task.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.ReconcilePublications(ctx); err != nil {
			t.Fatal(err)
		}
		items, _ := s.ListPublications(ctx, task.ID)
		if len(items) != 0 {
			t.Fatal("intermediate report without a delivery", state, items)
		}
	}
	// A temporary blocker is internal, even when it has a structured report.
	if _, err = s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/321"); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	next, _ := s.ListPublications(ctx, task.ID)
	if len(next) != 1 {
		t.Fatal("duplicate milestone")
	}
	// Later runs provide different text but must not rewrite a delivered PR report.
	result.TaskUpdate.Validation = "another test attempt"
	raw, _ = json.Marshal(result)
	if _, err := s.db.Exec(`UPDATE run SET output=? WHERE run_id=?`, string(raw), run.ID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"RUNNING", "BLOCKED", "WAITING_ENVIRONMENT", "WAITING_AUTHORIZATION", "WAITING_REVIEW"} {
		if _, err := s.db.Exec(`UPDATE task SET state=? WHERE task_id=?`, state, task.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.ReconcilePublications(ctx); err != nil {
			t.Fatal(err)
		}
		after, _ := s.ListPublications(ctx, task.ID)
		if !reflect.DeepEqual(next, after) {
			t.Fatal("intermediate state changed publication", state)
		}
	}
	if _, err := s.db.Exec(`UPDATE task SET state='COMPLETED' WHERE task_id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.ReconcilePublications(ctx); err != nil {
			t.Fatal(err)
		}
		after, _ := s.ListPublications(ctx, task.ID)
		if len(after) != 2 {
			t.Fatal("completion milestone missing or duplicated", after)
		}
	}
	refs, _, err := taskReferences(ctx, s.db, task.ID)
	if err != nil || len(refs) != 2 {
		t.Fatal(refs, err)
	}
	other, _, err := s.CreateTask(ctx, "", "other", "other")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindGitHubIssue(ctx, other.ID, source.ID, url); !errors.Is(err, model.ErrConflict) {
		t.Fatal("rebound another task", err)
	}
	if err = s.BindGitHubIssue(ctx, task.ID, source.ID, "https://evil.example/issues/123"); err == nil {
		t.Fatal("external endpoint accepted")
	}
}

func TestIssueLegacyDeliveryDoesNotBackfill(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	source := saveTestSource(t, s, "github")
	url := "https://github.com/oceanbase/seekdb/issues/123"
	if err := s.BindGitHubIssue(ctx, task.ID, source.ID, url); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	developmentFinish(t, s, run, 1, workflow.Result{Outcome: "blocked", Message: "waiting for tests", Artifacts: []workflow.File{}, TaskUpdate: &workflow.TaskUpdate{Kind: "bug", Approach: "fix"}})
	if _, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/321"); err != nil {
		t.Fatal(err)
	}
	old := model.Publication{Key: "legacy-progress", TaskID: task.ID, Platform: "github", URL: url, Body: "工作助手处理进展\n关联交付：\n- PR：https://github.com/oceanbase/seekdb/pull/321\n", State: "SYNCED", Version: 1, RemoteID: "old"}
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return savePublicationTx(ctx, tx, old) }); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListPublications(ctx, task.ID)
	if err := s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ListPublications(ctx, task.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("legacy delivery changed or backfilled", after)
	}
}

func TestIssuePlansPublishOnlyAfterHumanApproval(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, _ := developmentFixture(t)
	ant := saveTestSource(t, s, "antmultica")
	targets, _ := s.ListSourceTargets(ctx)
	pollStore(t, s, ant, targets[0], "", model.SourceEvent{Key: "issue:one:v1", Kind: "antmultica.issue", Entity: "antmultica:workspace:one", Title: "SEEK-1 request", Message: "Original request", URL: "https://antmultica.alipay.com/seekdb/issues/one"})
	events, _ := s.ListSourceEvents(ctx)
	task, err := s.GetTask(ctx, events[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	github := saveTestSource(t, s, "github")
	if err := s.BindGitHubIssue(ctx, task.ID, github.ID, "https://github.com/oceanbase/seekdb/issues/123"); err != nil {
		t.Fatal(err)
	}
	// Already published comments are historical data, not candidates for cleanup.
	old := model.Publication{Key: "historical-draft", TaskID: task.ID, Platform: "antmultica", Body: "old unapproved proposal", State: "SYNCED", Version: 1, AppliedVersion: 1, RemoteID: "old-comment"}
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return savePublicationTx(ctx, tx, old) }); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListPublications(ctx, task.ID)
	checkCount := func(want int) []model.Publication {
		t.Helper()
		if err := s.ReconcilePublications(ctx); err != nil {
			t.Fatal(err)
		}
		p, err := s.ListPublications(ctx, task.ID)
		if err != nil || len(p) != want {
			t.Fatalf("publications=%d want=%d err=%v", len(p), want, err)
		}
		if !reflect.DeepEqual(p[0], before[0]) {
			t.Fatal("historical comment changed")
		}
		return p
	}
	plan := func(name string) workflow.Result {
		r := submittedPlan(name)
		r.TaskUpdate = &workflow.TaskUpdate{Kind: "bug", Analysis: name, Approach: name, Reason: "repair root cause", Validation: "not tested"}
		return r
	}
	review := func(seq int64, decision string) {
		t.Helper()
		if err := s.RoutePlanReviews(ctx); err != nil {
			t.Fatal(err)
		}
		d := developmentState(t, s, task.ID)
		child, err := s.GetTask(ctx, d.ReviewerTaskID)
		if err != nil {
			t.Fatal(err)
		}
		r := startWork(t, s, reviewer, child)
		developmentFinish(t, s, r, seq, workflow.Result{Outcome: "review", ReviewDecision: decision, Message: decision, Artifacts: []workflow.File{}})
	}
	first := startWork(t, s, dev, task)
	checkCount(1) // Planning.
	developmentFinish(t, s, first, 1, plan("draft one"))
	checkCount(1) // Agent review.
	review(2, "changes_requested")
	checkCount(1) // Returned for changes.
	second := startWork(t, s, dev, task)
	developmentFinish(t, s, second, 3, plan("draft two"))
	review(4, "passed")
	checkCount(1) // Agent approval alone is insufficient.
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "CHANGES_REQUESTED", "revise"); err != nil {
		t.Fatal(err)
	}
	checkCount(1)
	third := startWork(t, s, dev, task)
	developmentFinish(t, s, third, 5, plan("final approved proposal"))
	review(6, "passed")
	w, _ = s.GetWorkDetail(ctx, task.ID)
	if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", "approved"); err != nil {
		t.Fatal(err)
	}
	p := checkCount(3) // One final-plan comment per source.
	for _, item := range p[1:] {
		if !strings.Contains(item.Body, "final approved proposal") || strings.Contains(item.Body, "draft two") || !strings.Contains(item.Body, "人工确认") {
			t.Fatal("wrong plan published", item)
		}
	}
	implementation := startWork(t, s, dev, task)
	if after := checkCount(3); !reflect.DeepEqual(p, after) {
		t.Fatal("starting implementation republished the plan")
	}
	if _, err := s.RegisterPR(ctx, task.ID, github.ID, "https://github.com/oceanbase/seekdb/pull/321"); err != nil {
		t.Fatal(err)
	}
	if after := checkCount(3); !reflect.DeepEqual(p, after) {
		t.Fatal("PR link change rewrote the plan")
	}
	developmentFinish(t, s, implementation, 7, workflow.Result{Outcome: "blocked", Message: "test environment unavailable", Artifacts: []workflow.File{}, TaskUpdate: &workflow.TaskUpdate{Kind: "bug", Analysis: "implemented analysis", Approach: "implemented fix", Reason: "root cause", Validation: "tests pending", BlockedReason: "test environment unavailable"}})
	p = checkCount(5)
	for _, item := range p[3:] {
		if !strings.Contains(item.Body, "/pull/321") || !strings.Contains(item.Body, "test environment unavailable") {
			t.Fatal("implementation updates suppressed", item)
		}
	}
	checkCount(5)
}

func TestIssuePlanPublicationLegacyAndFastImplementation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "implementation_before_poll", true: "historical_approval_no_backfill"}[legacy], func(t *testing.T) {
			ctx := context.Background()
			s, dev, reviewer, task := developmentFixture(t)
			source := saveTestSource(t, s, "github")
			if err := s.BindGitHubIssue(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/issues/123"); err != nil {
				t.Fatal(err)
			}
			r := startWork(t, s, dev, task)
			plan := submittedPlan("approved plan")
			plan.TaskUpdate = &workflow.TaskUpdate{Kind: "feature", Approach: "approved approach"}
			developmentFinish(t, s, r, 1, plan)
			if err := s.RoutePlanReviews(ctx); err != nil {
				t.Fatal(err)
			}
			d := developmentState(t, s, task.ID)
			child, _ := s.GetTask(ctx, d.ReviewerTaskID)
			rr := startWork(t, s, reviewer, child)
			developmentFinish(t, s, rr, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "passed", Artifacts: []workflow.File{}})
			w, _ := s.GetWorkDetail(ctx, task.ID)
			if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", "approved"); err != nil {
				t.Fatal(err)
			}
			if legacy {
				// A pre-upgrade persisted approval has no opt-in flag.
				if _, err := s.db.Exec(`UPDATE development SET data_json=json_remove(data_json,'$.publish_approved_plan') WHERE task_id=?`, task.ID); err != nil {
					t.Fatal(err)
				}
				before := developmentState(t, s, task.ID)
				if err := s.ReconcilePublications(ctx); err != nil {
					t.Fatal(err)
				}
				p, _ := s.ListPublications(ctx, task.ID)
				if len(p) != 0 || !reflect.DeepEqual(before, developmentState(t, s, task.ID)) {
					t.Fatal("historical approval backfilled or mutated")
				}
				return
			}
			impl := startWork(t, s, dev, task)
			developmentFinish(t, s, impl, 3, workflow.Result{Outcome: "blocked", Message: "needs environment", Artifacts: []workflow.File{}, TaskUpdate: &workflow.TaskUpdate{Kind: "feature", Approach: "implementation output", BlockedReason: "needs environment"}})
			if err := s.ReconcilePublications(ctx); err != nil {
				t.Fatal(err)
			}
			p, _ := s.ListPublications(ctx, task.ID)
			if len(p) != 1 || !strings.Contains(p[0].Body, "approved approach") || strings.Contains(p[0].Body, "implementation output") {
				t.Fatal("latest implementation replaced approved plan", p)
			}
		})
	}
}
