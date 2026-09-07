package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func qaTestFixture(t *testing.T) (*Store, model.AgentProfile, model.Task, model.Run, model.Task, model.Run, model.SourceTarget) {
	t.Helper()
	ctx := context.Background()
	s, author, parent := workFixture(t)
	first := startWork(t, s, author, parent)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: author.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: first.ID, SessionID: first.SessionID, Type: "session.bound", AgentSessionRef: "codex:original-work-memory"}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, first, 2, "review", "implementation")
	role := publishTestRole(t, s, "qa.review")
	qa, err := s.CreateAgent(ctx, model.AgentProfile{Name: "QA", RoleID: role.ID, RuntimeID: author.RuntimeID, AdapterID: author.AdapterID, ModelID: author.ModelID, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	source := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, parent.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/123")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	pollStore(t, s, source, target, head, model.SourceEvent{Key: "head:" + head, Kind: "github.head", HeadSHA: head, Message: "broad change"})
	reviews, err := s.ListSourceReviews(ctx)
	if err != nil || len(reviews) != 1 {
		t.Fatal(reviews, err)
	}
	child, err := s.GetTask(ctx, reviews[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, qa, child)
	return s, author, parent, first, child, run, targetByID(t, s, target.ID)
}
func testSubmission(t *testing.T, s *Store, run model.Run, seq int64, request workflow.TestRequest) {
	t.Helper()
	raw, err := json.Marshal(workflow.Result{Outcome: "review", Message: "Core persistence changes require regression", Artifacts: []workflow.File{{Name: "qa.md", Content: "Risks and test plan"}}, Summary: &workflow.CompletionSummary{Result: "QA evidence delivered", Learnings: []string{}, Improvements: []string{}}, TestRequests: []workflow.TestRequest{request}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(context.Background(), model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: seq, RunID: run.ID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
}
func initialTestRequest(target model.SourceTarget) workflow.TestRequest {
	return workflow.TestRequest{Kind: model.SeekDBTestKind, PRURL: target.Entity, HeadSHA: target.HeadSHA, Reason: "Core persistence changes affect recovery and compatibility"}
}
func oneTest(t *testing.T, s *Store, taskID string) model.TestPipeline {
	t.Helper()
	rows, err := s.ListTestPipelines(context.Background(), taskID)
	if err != nil || len(rows) == 0 {
		t.Fatal(rows, err)
	}
	return rows[0]
}
func attachTest(t *testing.T, s *Store, p model.TestPipeline, pipelineID int64) model.TestPipeline {
	t.Helper()
	ctx := context.Background()
	claimed, err := s.ClaimTestAction(ctx, p.ID)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	if err = s.AttachTestPipeline(ctx, p.ID, model.PipelineObservation{ID: pipelineID, Ref: model.SeekDBTestRef, SHA: strings.Repeat("b", 40), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	return oneTest(t, s, p.TaskID)
}
func observeTest(t *testing.T, s *Store, p model.TestPipeline, status, key string, apply bool) {
	t.Helper()
	ctx := context.Background()
	source, err := s.GetTaskSource(ctx, model.SeekDBTestSource)
	if err != nil {
		t.Fatal(err)
	}
	target := targetByID(t, s, p.PollTargetID)
	o := model.PipelineObservation{ID: p.PipelineID, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: status, URL: p.URL, Jobs: []model.PipelineJob{{ID: 111, Name: "recovery", Status: "failed", FailureReason: "script_failure", URL: "https://gitlab.oceanbase-dev.com/obqa/seekdb_test/-/jobs/111"}}}
	if err = s.CommitSourcePoll(ctx, source, target, json.RawMessage(`{"status":"`+status+`"}`), p.HeadSHA, []model.SourceEvent{{Key: key, Kind: "gitlab.pipeline", Message: "pipeline observation", HeadSHA: p.HeadSHA, Pipeline: &o}}, 0, model.PipelineFinished(status)); err != nil {
		t.Fatal(err)
	}
	if apply {
		if err = s.ProcessSourceEvents(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQAPipelineLifecyclePersistsBeforeQACompletionAndReturnsOriginalSession(t *testing.T) {
	ctx := context.Background()
	s, author, parent, first, child, qaRun, target := qaTestFixture(t)
	testSubmission(t, s, qaRun, 3, initialTestRequest(target))
	p := oneTest(t, s, parent.ID)
	if p.State != "QUEUED" || p.RequestedByTaskID != child.ID || p.HeadSHA != target.HeadSHA {
		t.Fatal(p)
	}
	done, err := s.GetTask(ctx, child.ID)
	if err != nil || done.State != model.TaskStateCompleted {
		t.Fatal(done, err)
	}
	if len(oneTest(t, s, child.ID).ID) == 0 {
		t.Fatal("reviewer lost original task test history")
	}
	// A duplicate report does not create another action or increment attempts.
	if err = s.sourceWrite(ctx, func(tx *sql.Tx) error {
		return requestTestPipelineTx(ctx, tx, child.ID, qaRun.ID, initialTestRequest(target))
	}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListTestPipelines(ctx, parent.ID)
	if len(all) != 1 {
		t.Fatal(all)
	}
	p = attachTest(t, s, p, 12345)
	if err = s.sourceWrite(ctx, func(tx *sql.Tx) error { return guardTestPipelinesTx(ctx, tx, parent.ID) }); !errors.Is(err, model.ErrConflict) {
		t.Fatal("active pipeline passed acceptance", err)
	}
	before, _ := s.GetTask(ctx, parent.ID)
	observeTest(t, s, p, "failed", "failed:1", false)
	after, _ := s.GetTask(ctx, parent.ID)
	if before.Version != after.Version || oneTest(t, s, parent.ID).State != "created" {
		t.Fatal("poller executed work instead of recording an event")
	}
	// Disable collection after acceptance of an event: Manager still delivers it.
	source, _ := s.GetTaskSource(ctx, model.SeekDBTestSource)
	source.Enabled = false
	if _, err = s.SaveTaskSource(ctx, source, source.Version); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if got := oneTest(t, s, parent.ID); got.State != "failed" || len(got.Jobs) != 1 {
		t.Fatal(got)
	}
	if err = s.CollectSourceReviews(ctx); err != nil {
		t.Fatal(err)
	}
	continued := startWork(t, s, author, parent)
	if continued.SessionID != first.SessionID || continued.AgentID != first.AgentID {
		t.Fatal("failure lost worker/session affinity")
	}
	session, _ := s.GetTaskSession(ctx, parent.ID)
	if session.AgentSessionRef != "codex:original-work-memory" {
		t.Fatal(session)
	}
	var instructions string
	if err = s.db.QueryRow(`SELECT json_extract(params_json,'$.instructions') FROM outbox_message WHERE method='run.start' AND json_extract(params_json,'$.run_id')=?`, continued.ID).Scan(&instructions); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions, p.ID) || !strings.Contains(instructions, "script_failure") {
		t.Fatal("worker did not receive evidence/retry identity")
	}
	retry := initialTestRequest(target)
	retry.RetryOf = p.ID
	retry.Reason = "Transient infrastructure issue confirmed; rerun exact SHA"
	testSubmission(t, s, continued, 4, retry)
	next := oneTest(t, s, parent.ID)
	if next.Attempt != 2 || next.RetryOf != p.ID || next.State != "QUEUED" {
		t.Fatal(next)
	}
	work, _ := s.GetWorkDetail(ctx, parent.ID)
	if _, err = s.DecideReview(ctx, parent.ID, work.Reviews[0].ID, "APPROVED", ""); !errors.Is(err, model.ErrConflict) {
		t.Fatal("incomplete tests allowed human approval", err)
	}
	source.Enabled = true
	source.Version++
	if _, err = s.SaveTaskSource(ctx, source, source.Version); err != nil {
		t.Fatal(err)
	}
	next = attachTest(t, s, next, 12346)
	observeTest(t, s, next, "success", "success:1", true)
	final := startWork(t, s, author, parent)
	finishWork(t, s, final, 5, "review", "all evidence ready")
	work, _ = s.GetWorkDetail(ctx, parent.ID)
	if _, err = s.DecideReview(ctx, parent.ID, work.Reviews[0].ID, "APPROVED", ""); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ListTestPipelines(ctx, parent.ID)
	if len(all) != 2 || all[1].PipelineID != 12345 || all[0].PipelineID != 12346 {
		t.Fatal("retry overwrote history", all)
	}
}

func TestPipelineSubmitUncertaintyBackupAndPausedFailure(t *testing.T) {
	ctx := context.Background()
	s, _, parent, first, _, run, target := qaTestFixture(t)
	testSubmission(t, s, run, 3, initialTestRequest(target))
	p := oneTest(t, s, parent.ID)
	claimed, err := s.ClaimTestAction(ctx, p.ID)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	copy := filepath.Join(t.TempDir(), "backup.sqlite")
	if err = s.Backup(ctx, copy); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(copy)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if oneTest(t, restored, parent.ID).State != "SUBMITTING" {
		t.Fatal("submission lost across restart")
	}
	if yes, err := restored.ClaimTestAction(ctx, p.ID); err != nil || yes {
		t.Fatal("uncertain action was claimed for a second POST", yes, err)
	}
	if err = restored.DeferTestAction(ctx, p.ID, "response uncertain"); err != nil {
		t.Fatal(err)
	}
	if err = restored.AttachTestPipeline(ctx, p.ID, model.PipelineObservation{ID: 8, Ref: model.SeekDBTestRef, SHA: strings.Repeat("b", 40)}); err != nil {
		t.Fatal(err)
	}
	if err = restored.AttachTestPipeline(ctx, p.ID, model.PipelineObservation{ID: 8, Ref: model.SeekDBTestRef, SHA: strings.Repeat("b", 40)}); err != nil {
		t.Fatal(err)
	}
	p = oneTest(t, restored, parent.ID)
	if err = restored.PauseWork(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	observeTest(t, restored, p, "failed", "failure", true)
	work, _ := restored.GetWorkConfig(ctx, parent.ID)
	paused, _ := restored.GetTask(ctx, parent.ID)
	session, _ := restored.GetTaskSession(ctx, parent.ID)
	if !work.Paused || paused.State != model.TaskStatePaused || session.ID != first.SessionID {
		t.Fatal("pipeline event unpaused or replaced session")
	}
	before, _ := restored.GetWorkDetail(ctx, parent.ID)
	if err = restored.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := restored.GetWorkDetail(ctx, parent.ID)
	if len(after.Messages) != len(before.Messages) {
		t.Fatal("duplicate terminal feedback")
	}
}

func TestPipelineAuthorizationStaleHeadsAndRetryLimit(t *testing.T) {
	ctx := context.Background()
	s, _, parent, first, child, run, target := qaTestFixture(t)
	request := initialTestRequest(target)
	check := func(taskID, runID string, r workflow.TestRequest, want bool) {
		t.Helper()
		err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return requestTestPipelineTx(ctx, tx, taskID, runID, r) })
		if (err == nil) != want {
			t.Fatalf("allowed=%v err=%v", want, err)
		}
	}
	check(parent.ID, first.ID, request, false) // developer cannot invent first QA requirement
	stale := request
	stale.HeadSHA = strings.Repeat("c", 40)
	check(child.ID, run.ID, stale, false)
	check(child.ID, run.ID, request, true)
	for attempt := 1; attempt <= 3; attempt++ {
		p := oneTest(t, s, parent.ID)
		if p.Attempt != attempt {
			t.Fatal(p)
		}
		if err := s.FailTestAction(ctx, p.ID, "ERROR", "GitLab rejected test"); err != nil {
			t.Fatal(err)
		}
		request.RetryOf = p.ID
		check(parent.ID, first.ID, request, attempt < 3)
	}
	// An old successful result remains history, never current-head evidence.
	p := oneTest(t, s, parent.ID)
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { p.State = "success"; return saveTestPipelineTx(ctx, tx, p) }); err != nil {
		t.Fatal(err)
	}
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		tgt, err := sourceTargetTx(ctx, tx, target.ID)
		if err != nil {
			return err
		}
		tgt.HeadSHA = strings.Repeat("d", 40)
		return saveSourceTargetTx(ctx, tx, tgt)
	}); err != nil {
		t.Fatal(err)
	}
	if oneTest(t, s, parent.ID).Current {
		t.Fatal("old test marked current")
	}
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return guardTestPipelinesTx(ctx, tx, parent.ID) }); !errors.Is(err, model.ErrConflict) {
		t.Fatal("old SHA bypassed gate", err)
	}
}

func TestUncertainTestNeedsExplicitDelayedHumanConfirmation(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _, _, run, target := qaTestFixture(t)
	testSubmission(t, s, run, 3, initialTestRequest(target))
	p := oneTest(t, s, parent.ID)
	if ok, err := s.ClaimTestAction(ctx, p.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.ConfirmTestPipelineNotCreated(ctx, parent.ID, p.ID); !errors.Is(err, model.ErrConflict) {
		t.Fatal("reset live submission", err)
	}
	if _, err := s.db.Exec(`UPDATE test_pipeline SET data_json=json_set(data_json,'$.submitted_at_ms',?) WHERE request_id=?`, time.Now().Add(-3*time.Minute).UnixMilli(), p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmTestPipelineNotCreated(ctx, "unrelated-task", p.ID); !errors.Is(err, model.ErrConflict) {
		t.Fatal("reset another task", err)
	}
	if err := s.ConfirmTestPipelineNotCreated(ctx, parent.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if got := oneTest(t, s, parent.ID); got.State != "ERROR" || got.PipelineID != 0 {
		t.Fatal(got)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE event_type='TestPipelineNotCreatedConfirmed'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
