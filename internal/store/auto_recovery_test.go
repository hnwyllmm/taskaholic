package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func failWork(t *testing.T, s *Store, run model.Run, seq int64, message string) {
	t.Helper()
	if _, err := s.ApplyRuntimeEvent(context.Background(), model.RuntimeEvent{
		RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: seq,
		RunID: run.ID, TaskID: run.TaskID, Type: "run.failed", Error: message,
	}); err != nil {
		t.Fatal(err)
	}
}

func addExhaustedPipeline(t *testing.T, s *Store, task model.Task) {
	t.Helper()
	ctx := context.Background()
	source := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/987")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	pollStore(t, s, source, target, head)
	p := model.TestPipeline{ID: "exhausted-pipeline", TaskID: task.ID, PRTargetID: target.ID, PRURL: target.Entity, HeadSHA: head, Kind: model.SeekDBTestKind, Attempt: 3, State: "failed", PipelineID: 42, ConfigSHA: strings.Repeat("b", 40), AnalysisRequired: true, CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
	raw, _ := json.Marshal(p)
	if _, err = s.db.Exec(`INSERT INTO test_pipeline(request_id,task_id,pr_target_id,head_sha,attempt,state,next_attempt_ms,data_json) VALUES(?,?,?,?,?,?,0,?)`, p.ID, p.TaskID, p.PRTargetID, p.HeadSHA, p.Attempt, p.State, raw); err != nil {
		t.Fatal(err)
	}
}

func addActivePipeline(t *testing.T, s *Store, task model.Task) {
	t.Helper()
	ctx := context.Background()
	source := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/986")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("c", 40)
	pollStore(t, s, source, target, head)
	p := model.TestPipeline{ID: "active-pipeline", TaskID: task.ID, PRTargetID: target.ID, PRURL: target.Entity, HeadSHA: head, Kind: model.SeekDBTestKind, Attempt: 1, State: "running", PipelineID: 43, ConfigSHA: strings.Repeat("d", 40), CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
	raw, _ := json.Marshal(p)
	if _, err = s.db.Exec(`INSERT INTO test_pipeline(request_id,task_id,pr_target_id,head_sha,attempt,state,next_attempt_ms,data_json) VALUES(?,?,?,?,?,?,0,?)`, p.ID, p.TaskID, p.PRTargetID, p.HeadSHA, p.Attempt, p.State, raw); err != nil {
		t.Fatal(err)
	}
}

func automaticRecoveryMessageForTask(t *testing.T, s *Store, taskID string) model.TaskMessage {
	t.Helper()
	message, err := scanMessage(s.db.QueryRow(messageSelect+` WHERE task_id=? AND delivery='PENDING' ORDER BY seq DESC LIMIT 1`, taskID))
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func makeRecoveryDue(t *testing.T, s *Store, taskID string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE task_workflow SET retry_at_ms=0 WHERE task_id=?`, taskID); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticRecoverySkipsHistoricalBlockedWork(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	run := startWork(t, s, agent, task)
	failWork(t, s, run, 1, "compiler exited with status 1")
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 0 {
		t.Fatalf("historical work recovered: count=%d err=%v", recovered, err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if current.State != model.TaskStateBlocked {
		t.Fatal(current.State)
	}
}

func TestAutomaticRecoveryQueuesOriginalAgentAndSession(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	failWork(t, s, run, 1, "official build script failed")
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 1 {
		t.Fatalf("count=%d err=%v", recovered, err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	config, _ := s.GetWorkConfig(ctx, task.ID)
	var retryAt int64
	if err := s.db.QueryRow(`SELECT retry_at_ms FROM task_workflow WHERE task_id=?`, task.ID).Scan(&retryAt); err != nil {
		t.Fatal(err)
	}
	message := automaticRecoveryMessageForTask(t, s, task.ID)
	if current.State != model.TaskStateQueued || config.Paused || config.SchedulerError != "" || retryAt <= time.Now().UnixMilli() ||
		!strings.Contains(message.Content, "系统自动解阻") || !strings.Contains(message.Content, "原 Agent、原 Session") {
		t.Fatalf("task=%+v config=%+v retry=%d message=%+v", current, config, retryAt, message)
	}
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 0 {
		t.Fatalf("duplicate recovery: count=%d err=%v", recovered, err)
	}
	makeRecoveryDue(t, s, task.ID)
	next := startWork(t, s, agent, task)
	if next.AgentID != run.AgentID || next.SessionID != run.SessionID {
		t.Fatalf("affinity changed: before=%+v after=%+v", run, next)
	}
}

func TestAutomaticRecoveryHandsOffExhaustedNativeCodexSession(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	first := startWork(t, s, agent, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: first.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1,
		RunID: first.ID, TaskID: task.ID, SessionID: first.SessionID,
		Type: "session.bound", AgentSessionRef: "codex:exhausted-native-thread",
	}); err != nil {
		t.Fatal(err)
	}
	failWork(t, s, first, 2, "Codex ran out of room in the model's context window. Start a new thread or clear earlier history before retrying.")
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 1 {
		t.Fatalf("count=%d err=%v", recovered, err)
	}

	before, err := s.GetTaskSession(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID != first.SessionID || before.AgentSessionRef != "" || !strings.Contains(string(before.Metadata), "native_context_handoff") {
		t.Fatalf("native handoff must retain logical session and clear only native ref: %+v", before)
	}
	message := automaticRecoveryMessageForTask(t, s, task.ID)
	if !strings.Contains(message.Content, "新的原生会话") || !strings.Contains(message.Content, "受限交接快照") || !strings.Contains(message.Content, task.Title) {
		t.Fatalf("missing handoff snapshot: %s", message.Content)
	}
	var activeBindings int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM task_session WHERE task_id=? AND unbound_at_ms IS NULL`, task.ID).Scan(&activeBindings); err != nil || activeBindings != 1 {
		t.Fatalf("logical session binding changed: count=%d err=%v", activeBindings, err)
	}

	makeRecoveryDue(t, s, task.ID)
	next := startWork(t, s, agent, task)
	if next.SessionID != first.SessionID || next.AgentID != first.AgentID {
		t.Fatalf("handoff changed task affinity: before=%+v after=%+v", first, next)
	}
	spec := outboxSpec(t, s, next.ID)
	if spec.AgentSessionRef != "" || !strings.Contains(spec.Instructions, "新的原生会话") || !strings.Contains(spec.Instructions, "原隔离工作目录") {
		t.Fatalf("next run did not start a fresh native session with snapshot: %+v", spec)
	}
}

func TestAutomaticRecoveryCorrectsInvalidResultWithoutReplayingIt(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1,
		RunID: run.ID, TaskID: task.ID, Type: "run.completed", Output: "not-json",
	}); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 1 {
		t.Fatalf("count=%d err=%v", recovered, err)
	}
	message := automaticRecoveryMessageForTask(t, s, task.ID)
	if !strings.Contains(message.Content, "result_protocol") || !strings.Contains(message.Content, "结果") {
		t.Fatal(message.Content)
	}
	var materialized int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM artifact WHERE task_id=? AND run_id=?`, task.ID, run.ID).Scan(&materialized); err != nil || materialized != 0 {
		t.Fatal("invalid output was materialized", materialized, err)
	}
}

func TestAutomaticRecoveryPreservesApprovedDevelopment(t *testing.T) {
	ctx := context.Background()
	s, developer, task, run := approvedEnvironmentFixture(t)
	before := *developmentState(t, s, task.ID)
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	failWork(t, s, run, 3, "build.ps1 returned 1")
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 1 {
		t.Fatalf("count=%d err=%v", recovered, err)
	}
	after := developmentState(t, s, task.ID)
	if after.Phase != "IMPLEMENTING" || after.PlanHash != before.PlanHash || after.ApprovedReviewID != before.ApprovedReviewID || after.Version != before.Version {
		t.Fatalf("approval changed: before=%+v after=%+v", before, after)
	}
	message := automaticRecoveryMessageForTask(t, s, task.ID)
	if !strings.Contains(message.Content, "由你定位、修复") || !strings.Contains(message.Content, "Manager 不代写业务修复") {
		t.Fatal(message.Content)
	}
	makeRecoveryDue(t, s, task.ID)
	next := startWork(t, s, developer, task)
	if next.SessionID != run.SessionID || outboxSpec(t, s, next.ID).ExecutionGrant == nil {
		t.Fatal("recovery lost development session or grant")
	}
}

func TestAutomaticRecoveryDoesNotLetUnrelatedActivePipelineHideRuntimeFailure(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	addActivePipeline(t, s, task)
	failWork(t, s, run, 1, "runtime connection reset")
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 1 {
		t.Fatalf("active pipeline hid an unrelated runtime recovery: count=%d err=%v", recovered, err)
	}
}

func TestAutomaticRecoveryDoesNotBypassGates(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	failWork(t, s, run, 1, "permission required")
	if _, err := s.db.Exec(`INSERT INTO permission_request(request_id,task_id,fingerprint,state,data_json) VALUES('permission',?,'permission-fingerprint','PENDING','{}')`, task.ID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 0 {
		t.Fatalf("permission gate bypassed: count=%d err=%v", recovered, err)
	}
	if _, err := s.db.Exec(`DELETE FROM permission_request WHERE request_id='permission'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO publication(publication_key,task_id,state,next_attempt_ms,data_json) VALUES('publication',?,'UNCERTAIN',0,'{}')`, task.ID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 0 {
		t.Fatalf("publication gate bypassed: count=%d err=%v", recovered, err)
	}
}

func TestAutomaticRecoveryDoesNotUndoUserPause(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	if err := s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	// A Runtime can race the interrupt directive and report failed instead of
	// interrupted. The explicit user hold must still win.
	failWork(t, s, run, 1, "terminated while applying interrupt")
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 0 {
		t.Fatalf("user pause was undone: count=%d err=%v", recovered, err)
	}
	config, _ := s.GetWorkConfig(ctx, task.ID)
	if !config.Paused {
		t.Fatal("user pause was cleared")
	}
}

func TestAutomaticRecoveryHonorsPauseAfterFailureBeforeSweep(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	if err := s.EnableAutomaticRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	failWork(t, s, run, 1, "compiler failed")
	// Run failure has already set paused=1, but this API call is a separate,
	// explicit user hold and must not be lost as an apparent no-op.
	if err := s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 0 {
		t.Fatalf("explicit hold was undone: count=%d err=%v", recovered, err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if current.State != model.TaskStatePaused {
		t.Fatal(current.State)
	}
}

func TestAutomaticRecoveryBackoffHasNoAttemptBudget(t *testing.T) {
	if got := automaticRecoveryDelay("run_failure", "quota exceeded", 1000); got != 5*time.Minute {
		t.Fatal(got)
	}
	if got := automaticRecoveryDelay("result_protocol", "invalid json", 1000); got != 30*time.Second {
		t.Fatal(got)
	}
}

func TestExhaustedPipelineStopsAgentBlockedLoopsButNotRuntimeFailures(t *testing.T) {
	t.Run("runtime failure", func(t *testing.T) {
		ctx := context.Background()
		s, agent, task := workFixture(t)
		if err := s.EnableAutomaticRecovery(ctx); err != nil {
			t.Fatal(err)
		}
		run := startWork(t, s, agent, task)
		addExhaustedPipeline(t, s, task)
		failWork(t, s, run, 1, "Selected model is at capacity")
		if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 1 {
			t.Fatalf("runtime failure was hidden by pipeline analysis gate: count=%d err=%v", recovered, err)
		}
	})
	t.Run("agent blocked", func(t *testing.T) {
		ctx := context.Background()
		s, agent, task := workFixture(t)
		if err := s.EnableAutomaticRecovery(ctx); err != nil {
			t.Fatal(err)
		}
		run := startWork(t, s, agent, task)
		addExhaustedPipeline(t, s, task)
		finishWork(t, s, run, 1, "blocked", "pipeline evidence still requires analysis")
		if recovered, err := s.AutoRecoverBlockedWork(ctx); err != nil || recovered != 0 {
			t.Fatalf("agent blocked result kept looping after pipeline budget: count=%d err=%v", recovered, err)
		}
	})
}

func TestPlanReviewDoesNotStopAtArtificialRoundLimit(t *testing.T) {
	ctx := context.Background()
	s, developer, _, task := developmentFixture(t)
	run := startWork(t, s, developer, task)
	developmentFinish(t, s, run, 1, submittedPlan("mature plan"))
	if _, err := s.db.Exec(`UPDATE development SET data_json=json_set(data_json,'$.rounds',99) WHERE task_id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	if d.ReviewerTaskID == "" {
		t.Fatal("high round count stopped review routing")
	}
}
