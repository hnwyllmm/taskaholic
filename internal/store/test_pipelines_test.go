package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
func testSubmission(t *testing.T, s *Store, run model.Run, seq int64, request workflow.TestRequest, reviewDecision ...string) {
	t.Helper()
	decision := ""
	if len(reviewDecision) > 0 {
		decision = reviewDecision[0]
	}
	raw, err := json.Marshal(workflow.Result{Outcome: "review", ReviewDecision: decision, Message: "Core persistence changes require regression", Artifacts: []workflow.File{{Name: "qa.md", Content: "Risks and test plan"}}, Summary: &workflow.CompletionSummary{Result: "QA evidence delivered", Learnings: []string{}, Improvements: []string{}}, TestRequests: []workflow.TestRequest{request}})
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
func exhaustQuickRetries(t *testing.T, s *Store, p model.TestPipeline) model.TestPipeline {
	t.Helper()
	if err := s.sourceWrite(context.Background(), func(tx *sql.Tx) error {
		current, err := testPipelineTx(context.Background(), tx, p.ID)
		if err != nil {
			return err
		}
		current.QuickRetries = model.PipelineQuickRetryLimit
		return saveTestPipelineTx(context.Background(), tx, current)
	}); err != nil {
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
	jobID := int64(111 + p.QuickRetries)
	o := model.PipelineObservation{ID: p.PipelineID, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: status, URL: p.URL, Jobs: []model.PipelineJob{{ID: jobID, Name: "recovery", Status: "failed", FailureReason: "script_failure", URL: fmt.Sprintf("https://gitlab.oceanbase-dev.com/obqa/seekdb_test/-/jobs/%d", jobID), LogCollected: true, LogExcerpt: "server assertion failed"}}}
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
	testSubmission(t, s, qaRun, 3, initialTestRequest(target), "passed")
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
	if err := s.ReconcilePublications(ctx); err != nil {
		t.Fatal(err)
	}
	publications, err := s.ListPublications(ctx, parent.ID)
	if err != nil || len(publications) != 1 {
		t.Fatal(publications, err)
	}
	publication, err := s.ClaimPublication(ctx, publications[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPublication(ctx, publication, "101", publication.URL+"#issuecomment-101", "SYNCED", ""); err != nil {
		t.Fatal(err)
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
	p = exhaustQuickRetries(t, s, p)
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
	if next.ID != p.ID || next.Attempt != 1 || next.State != "failed" {
		t.Fatal("same SHA created a new full pipeline", next)
	}
	var fullRetryRejected int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='TestPipelineFullRetryRejected'`, parent.ID).Scan(&fullRetryRejected); err != nil || fullRetryRejected != 1 {
		t.Fatal("same SHA retry rejection was not auditable", fullRetryRejected, err)
	}
	if err = s.sourceWrite(ctx, func(tx *sql.Tx) error {
		current, err := sourceTargetTx(ctx, tx, target.ID)
		if err != nil {
			return err
		}
		current.HeadSHA = strings.Repeat("c", 40)
		return saveSourceTargetTx(ctx, tx, current)
	}); err != nil {
		t.Fatal(err)
	}
	newHeadTarget := targetByID(t, s, target.ID)
	newRequest := initialTestRequest(newHeadTarget)
	if err = s.sourceWrite(ctx, func(tx *sql.Tx) error { return requestTestPipelineTx(ctx, tx, parent.ID, continued.ID, newRequest) }); err != nil {
		t.Fatal(err)
	}
	next = oneTest(t, s, parent.ID)
	if next.Attempt != 1 || next.RetryOf != "" || next.State != "QUEUED" || next.HeadSHA != newHeadTarget.HeadSHA {
		t.Fatal("new SHA did not create a fresh pipeline", next)
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
	// A terminal pipeline result belongs to the root delivery gate. It must not
	// reopen the completed reviewer that requested it.
	reviewerAfterTest, err := s.GetTask(ctx, child.ID)
	if err != nil || reviewerAfterTest.State != model.TaskStateCompleted {
		t.Fatal("pipeline result reopened reviewer", reviewerAfterTest, err)
	}
	var reviewerPending int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM task_message WHERE task_id=? AND delivery='PENDING'`, child.ID).Scan(&reviewerPending); err != nil || reviewerPending != 0 {
		t.Fatal("pipeline result was delivered to reviewer", reviewerPending, err)
	}
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

func TestFailedPipelineWaitsForManagerEvidenceThenResumesOriginalSession(t *testing.T) {
	ctx := context.Background()
	s, author, parent, first, _, qaRun, target := qaTestFixture(t)
	testSubmission(t, s, qaRun, 3, initialTestRequest(target), "passed")
	p := attachTest(t, s, oneTest(t, s, parent.ID), 22345)
	p = exhaustQuickRetries(t, s, p)
	source, err := s.GetTaskSource(ctx, model.SeekDBTestSource)
	if err != nil {
		t.Fatal(err)
	}
	watch := targetByID(t, s, p.PollTargetID)
	observation := model.PipelineObservation{ID: p.PipelineID, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: "failed", URL: p.URL, Jobs: []model.PipelineJob{{ID: 221, Name: "windows", Status: "failed", FailureReason: "script_failure", URL: model.SeekDBTestHost + "/obqa/seekdb_test/-/jobs/221"}}}
	if err = s.CommitSourcePoll(ctx, source, watch, json.RawMessage(`{"status":"failed","seq":1}`), p.HeadSHA, []model.SourceEvent{{Key: "failed-without-log", Kind: "gitlab.pipeline", HeadSHA: p.HeadSHA, Pipeline: &observation}}, 0, true); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingTestEvidence(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != p.ID {
		t.Fatal(pending, err)
	}
	if err = s.AttachTestEvidence(ctx, p.ID, model.PipelineObservation{ID: p.PipelineID, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: "failed", Jobs: []model.PipelineJob{{ID: 221, Name: "windows", Status: "failed", FailureReason: "script_failure", URL: observation.Jobs[0].URL, LogCollected: true, LogExcerpt: "fatal: expected path was not opened"}}}); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingTestEvidence(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	continued := startWork(t, s, author, parent)
	if continued.SessionID != first.SessionID || continued.AgentID != first.AgentID {
		t.Fatal("evidence continuation lost worker/session affinity", continued)
	}
	var instructions string
	if err = s.db.QueryRow(`SELECT json_extract(params_json,'$.instructions') FROM outbox_message WHERE method='run.start' AND json_extract(params_json,'$.run_id')=?`, continued.ID).Scan(&instructions); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions, "fatal: expected path was not opened") || !strings.Contains(instructions, "凭据由 Manager 隔离保管") {
		t.Fatal("trusted evidence was not supplied to the original Agent")
	}
}

func TestHistoricalPipelineFailureSummaryRefreshesMySQLLogsAndReopensOriginalAnalysis(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _, _, qaRun, target := qaTestFixture(t)
	testSubmission(t, s, qaRun, 3, initialTestRequest(target), "passed")
	p := attachTest(t, s, oneTest(t, s, parent.ID), 27345)
	p = exhaustQuickRetries(t, s, p)
	observeTest(t, s, p, "failed", "legacy-summary", true)
	p = oneTest(t, s, parent.ID)
	if !model.PipelineFailureEvidenceReady(p.Jobs) || !p.AnalysisDelivered {
		t.Fatal("fixture did not create an already-delivered failure", p)
	}
	// Simulate a record written before the reader started preserving full
	// counts or every mysqltest trace. Its original prompt could not have
	// produced the per-job case analysis now required, so the compatibility
	// refresh must create exactly one better-evidence continuation.
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		current, err := testPipelineTx(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		current.FailureSummary = model.PipelineFailureSummary{}
		return saveTestPipelineTx(ctx, tx, current)
	}); err != nil {
		t.Fatal(err)
	}
	var messagesBefore int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_message WHERE task_id=?`, parent.ID).Scan(&messagesBefore); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingTestEvidence(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != p.ID {
		t.Fatal("legacy record was not selected for a count refresh", pending, err)
	}
	jobs := make([]model.PipelineJob, 30)
	for i := range jobs {
		name := "non-mysqltest"
		if i < 4 {
			name = "mysqltest-shard"
		}
		jobs[i] = model.PipelineJob{ID: int64(i + 1), Name: name, Status: "failed", LogCollected: true, LogExcerpt: "bounded evidence"}
	}
	observation := model.PipelineObservation{
		ID:     p.PipelineID,
		Ref:    model.SeekDBTestRef,
		SHA:    p.ConfigSHA,
		Status: "failed",
		Jobs:   jobs,
		FailureSummary: model.PipelineFailureSummary{
			TotalFailures:            31,
			MySQLTestFailures:        4,
			NonMySQLTestFailures:     27,
			CollectionComplete:       true,
			DetailTruncated:          true,
			MySQLTestDetailsKnown:    true,
			MySQLTestDetailsComplete: true,
		},
	}
	if err = s.AttachTestEvidence(ctx, p.ID, observation); err != nil {
		t.Fatal(err)
	}
	got := oneTest(t, s, parent.ID)
	if got.FailureSummary.TotalFailures != 31 || got.FailureSummary.MySQLTestFailures != 4 || got.FailureSummary.NonMySQLTestFailures != 27 || !got.FailureSummary.CollectionComplete || !got.FailureSummary.DetailTruncated {
		t.Fatal("full historical failure count was not persisted", got.FailureSummary)
	}
	if !got.AnalysisDelivered || got.State != "failed" || !mysqlTestLogsAvailable(got) {
		t.Fatal("backfill reopened the historical failure", got)
	}
	var messagesAfter int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM task_message WHERE task_id=?`, parent.ID).Scan(&messagesAfter); err != nil {
		t.Fatal(err)
	}
	if messagesAfter != messagesBefore+1 {
		t.Fatal("mysqltest evidence refresh did not create exactly one Agent continuation", messagesBefore, messagesAfter)
	}
	pending, err = s.PendingTestEvidence(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal("persisted full count should stop compatibility refresh", pending, err)
	}
}

func TestThreeSmallNonMySQLFailuresWaitForAgentThenRetryInPlace(t *testing.T) {
	ctx := context.Background()
	s, author, parent, first, _, qaRun, target := qaTestFixture(t)
	testSubmission(t, s, qaRun, 3, initialTestRequest(target), "passed")
	p := attachTest(t, s, oneTest(t, s, parent.ID), 32345)
	source, err := s.GetTaskSource(ctx, model.SeekDBTestSource)
	if err != nil {
		t.Fatal(err)
	}
	jobs := []model.PipelineJob{
		{ID: 11, Name: "compile", Status: "failed", LogCollected: true, LogExcerpt: "transient runner disconnect"},
		{ID: 12, Name: "package", Status: "failed", LogCollected: true, LogExcerpt: "transient runner disconnect"},
		{ID: 13, Name: "sanity", Status: "failed", LogCollected: true, LogExcerpt: "transient runner disconnect"},
	}
	observation := model.PipelineObservation{ID: p.PipelineID, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: "failed", URL: p.URL, Jobs: jobs, FailureSummary: model.SummarizePipelineFailures(jobs)}
	if err = s.CommitSourcePoll(ctx, source, targetByID(t, s, p.PollTargetID), json.RawMessage(`{"status":"failed"}`), p.HeadSHA, []model.SourceEvent{{Key: "three-small-failures", Kind: "gitlab.pipeline", HeadSHA: p.HeadSHA, Pipeline: &observation}}, 0, true); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	p = oneTest(t, s, parent.ID)
	if p.State != "failed" || !p.AnalysisRequired || !p.AnalysisDelivered || p.QuickRetries != 0 {
		t.Fatal("Manager retried before the development Agent made a decision", p)
	}
	continued := startWork(t, s, author, parent)
	if continued.AgentID != first.AgentID || continued.SessionID != first.SessionID {
		t.Fatal("pipeline analysis did not return to original Agent/session", continued)
	}
	result := workflow.Result{Outcome: "blocked", Message: "The three failures are a shared runner outage; retry the same Pipeline.", Artifacts: []workflow.File{}, PipelineFailureAssessment: &workflow.PipelineFailureAssessment{RequestID: p.ID, Relation: "unrelated", Decision: "retry", Reason: "All three failures stop before any changed code runs.", Evidence: "Each trace reports the same runner disconnect.", MySQLTestCaseCount: 0, MySQLTestJobs: []model.MySQLTestJobAssessment{}}}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: continued.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 4, RunID: continued.ID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
	p = oneTest(t, s, parent.ID)
	if p.State != "RETRY_QUEUED" || p.FailureAssessment == nil || p.FailureAssessment.Decision != "retry" || len(p.FailureHistory) != 1 || p.FailureHistory[0].Assessment == nil {
		t.Fatal("Agent-approved retry or audit record missing", p)
	}
	claimed, err := s.ClaimTestRetry(ctx, p.ID)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	p = oneTest(t, s, parent.ID)
	if err = s.CompleteTestRetry(ctx, p.ID, model.PipelineObservation{ID: p.PipelineID, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	p = oneTest(t, s, parent.ID)
	if p.State != "created" || p.PipelineID != 32345 || p.QuickRetries != 1 {
		t.Fatal("Agent-approved retry did not stay on the original Pipeline", p)
	}
}

func TestPipelineFailurePolicyAllowsAgentRequestedRetriesThroughThreeNonMySQLJobs(t *testing.T) {
	for _, count := range []int{1, 2, 3, 4} {
		summary := model.PipelineFailureSummary{TotalFailures: count, NonMySQLTestFailures: count, CollectionComplete: true}
		if got, want := summary.AllowsQuickRetry(), count <= model.PipelineQuickRetryMaxJobs; got != want {
			t.Fatalf("%d non-mysqltest failures retryable=%t, want %t", count, got, want)
		}
	}
	if (model.PipelineFailureSummary{TotalFailures: 1, MySQLTestFailures: 1, CollectionComplete: true}).AllowsQuickRetry() {
		t.Fatal("mysqltest retry must be decided from per-job case analysis, not the non-mysqltest shortcut")
	}
}

func TestMySQLTestFailureRequiresEveryJobCaseAnalysisBeforeAgentCanRetry(t *testing.T) {
	ctx := context.Background()
	s, author, parent, first, _, qaRun, target := qaTestFixture(t)
	testSubmission(t, s, qaRun, 3, initialTestRequest(target), "passed")
	p := attachTest(t, s, oneTest(t, s, parent.ID), 47345)
	jobs := []model.PipelineJob{
		{ID: 21, Name: "mysqltest-core", Status: "failed", LogCollected: true, LogExcerpt: "[fail] t/replay/a.test"},
		{ID: 22, Name: "mysqltest-compat", Status: "failed", LogCollected: true, LogExcerpt: "[fail] t/replay/b.test\n[fail] t/replay/c.test"},
	}
	observation := model.PipelineObservation{
		ID: p.PipelineID, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: "failed", URL: p.URL, Jobs: jobs,
		FailureSummary: model.PipelineFailureSummary{TotalFailures: 2, MySQLTestFailures: 2, CollectionComplete: true, MySQLTestDetailsKnown: true, MySQLTestDetailsComplete: true},
	}
	source, err := s.GetTaskSource(ctx, model.SeekDBTestSource)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CommitSourcePoll(ctx, source, targetByID(t, s, p.PollTargetID), json.RawMessage(`{"status":"failed"}`), p.HeadSHA, []model.SourceEvent{{Key: "mysqltest-failure", Kind: "gitlab.pipeline", HeadSHA: p.HeadSHA, Pipeline: &observation}}, 0, true); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	p = oneTest(t, s, parent.ID)
	if p.State != "failed" || !p.AnalysisDelivered || !mysqlTestLogsAvailable(p) {
		t.Fatal("mysqltest evidence was not delivered for analysis", p)
	}
	missingJob := workflow.PipelineFailureAssessment{RequestID: p.ID, Relation: "unrelated", Decision: "retry", Reason: "The affected paths are not in this change.", Evidence: "The first job shows a baseline fixture failure.", MySQLTestCaseCount: 1, MySQLTestJobs: []model.MySQLTestJobAssessment{{JobID: 21, JobName: "mysqltest-core", FailedCaseCount: 1, FailedCaseNames: []string{"t/replay/a.test"}}}}
	if err = s.sourceWrite(ctx, func(tx *sql.Tx) error {
		return recordPipelineFailureAssessmentTx(ctx, tx, parent.ID, first.ID, missingJob)
	}); !errors.Is(err, model.ErrValidation) {
		t.Fatal("partial mysqltest case analysis was accepted", err)
	}
	continued := startWork(t, s, author, parent)
	if continued.SessionID != first.SessionID {
		t.Fatal("mysqltest analysis lost original Session", continued)
	}
	result := workflow.Result{Outcome: "blocked", Message: "All three failed cases are baseline fixture failures; retry the original Pipeline.", Artifacts: []workflow.File{}, PipelineFailureAssessment: &workflow.PipelineFailureAssessment{RequestID: p.ID, Relation: "unrelated", Decision: "retry", Reason: "None of the case paths or traces reaches the changed code.", Evidence: "mysqltest-core has one baseline failure; mysqltest-compat has two in the same fixture family.", MySQLTestCaseCount: 3, MySQLTestJobs: []model.MySQLTestJobAssessment{{JobID: 21, JobName: "mysqltest-core", FailedCaseCount: 1, FailedCaseNames: []string{"t/replay/a.test"}}, {JobID: 22, JobName: "mysqltest-compat", FailedCaseCount: 2, FailedCaseNames: []string{"t/replay/b.test", "t/replay/c.test"}}}}}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: continued.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 4, RunID: continued.ID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
	got := oneTest(t, s, parent.ID)
	if got.State != "RETRY_QUEUED" || got.FailureAssessment == nil || got.FailureAssessment.MySQLTestCaseCount != 3 || len(got.FailureAssessment.MySQLTestJobs) != 2 || len(got.FailureHistory) != 1 || got.FailureHistory[0].Assessment == nil {
		t.Fatal("complete mysqltest analysis was not stored with the retry request", got)
	}
}

func TestPipelineFailureAssessmentIsStoredBesideCurrentFailure(t *testing.T) {
	ctx := context.Background()
	s, author, parent, first, _, qaRun, target := qaTestFixture(t)
	testSubmission(t, s, qaRun, 3, initialTestRequest(target), "passed")
	p := attachTest(t, s, oneTest(t, s, parent.ID), 52345)
	p = exhaustQuickRetries(t, s, p)
	observeTest(t, s, p, "failed", "assessment-failure", true)
	continued := startWork(t, s, author, parent)
	if continued.SessionID != first.SessionID {
		t.Fatal("failure assessment lost original session")
	}
	result := workflow.Result{
		Outcome:                   "needs_input",
		Message:                   "The failing test is outside the changed iterator path; please decide whether to retry after the shared runner recovers.",
		Artifacts:                 []workflow.File{{Name: "failure-analysis.md", Content: "Evidence and comparison."}},
		Summary:                   &workflow.CompletionSummary{Result: "Failure assessed", Learnings: []string{}, Improvements: []string{}},
		PipelineFailureAssessment: &workflow.PipelineFailureAssessment{RequestID: p.ID, Relation: "unrelated", Decision: "hold", Reason: "The failed persistence suite does not execute the changed replay iterator path.", Evidence: "The failure trace is in an unrelated fixture and the diff has no matching call path.", MySQLTestCaseCount: 0, MySQLTestJobs: []model.MySQLTestJobAssessment{}},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: continued.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 4, RunID: continued.ID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
	got := oneTest(t, s, parent.ID)
	if got.FailureAssessment == nil || got.FailureAssessment.Relation != "unrelated" || !strings.Contains(got.FailureAssessment.Reason, "iterator") {
		t.Fatal("pipeline assessment was not persisted", got)
	}
	task, err := s.GetTask(ctx, parent.ID)
	if err != nil || task.State != model.TaskStateInput {
		t.Fatal("unrelated failure did not surface a user decision", task, err)
	}
}

func TestActivePipelineIsShownAsWaitingTestsRatherThanBlocked(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _, _, qaRun, target := qaTestFixture(t)
	testSubmission(t, s, qaRun, 3, initialTestRequest(target), "passed")
	p := attachTest(t, s, oneTest(t, s, parent.ID), 62345)
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return setWorkStateTx(ctx, tx, parent.ID, model.TaskStateBlocked) }); err != nil {
		t.Fatal(err)
	}
	source, err := s.GetTaskSource(ctx, model.SeekDBTestSource)
	if err != nil {
		t.Fatal(err)
	}
	watch := targetByID(t, s, p.PollTargetID)
	observation := model.PipelineObservation{ID: p.PipelineID, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: "running", URL: p.URL}
	if err = s.CommitSourcePoll(ctx, source, watch, json.RawMessage(`{"status":"running"}`), p.HeadSHA, []model.SourceEvent{{Key: "running", Kind: "gitlab.pipeline", HeadSHA: p.HeadSHA, Pipeline: &observation}}, 0, false); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, parent.ID)
	if err != nil || got.State != model.TaskStateWaitingTests {
		t.Fatal("active pipeline remained blocked", got, err)
	}
}

func TestPipelineSubmitUncertaintyBackupAndPausedFailure(t *testing.T) {
	ctx := context.Background()
	s, _, parent, first, _, run, target := qaTestFixture(t)
	testSubmission(t, s, run, 3, initialTestRequest(target), "passed")
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
	p = exhaustQuickRetries(t, restored, p)
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

func TestPipelineAuthorizationStaleHeadsAndSameSHAFullRetryRejected(t *testing.T) {
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
	p := oneTest(t, s, parent.ID)
	if err := s.FailTestAction(ctx, p.ID, "ERROR", "GitLab rejected test"); err != nil {
		t.Fatal(err)
	}
	request.RetryOf = p.ID
	check(parent.ID, first.ID, request, true) // rejection is durably recorded
	if got := oneTest(t, s, parent.ID); got.ID != p.ID || got.Attempt != 1 {
		t.Fatal("same SHA retry created a pipeline", got)
	}
	var rejected int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='TestPipelineFullRetryRejected'`, parent.ID).Scan(&rejected); err != nil || rejected != 1 {
		t.Fatal("same SHA full retry rejection was not auditable", rejected, err)
	}
	// An old successful result remains history, never current-head evidence.
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
	testSubmission(t, s, run, 3, initialTestRequest(target), "passed")
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
