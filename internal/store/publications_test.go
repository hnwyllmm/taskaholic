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
	for _, q := range []string{`DROP TABLE development_run`, `DROP TABLE development`, `DROP TABLE publication_history`, `DROP TABLE publication`, `DELETE FROM schema_version WHERE version>=16`} {
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
	if err != nil || len(p) != 1 || p[0].URL != url || p[0].Sticky || !strings.Contains(p[0].Body, "missing affected version") {
		t.Fatal(p, err)
	}
	if err = s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	next, _ := s.ListPublications(ctx, task.ID)
	if len(next) != 1 {
		t.Fatal("duplicate milestone")
	}
	refs, _, err := taskReferences(ctx, s.db, task.ID)
	if err != nil || len(refs) != 1 || refs[0].URL != url {
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
