package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func developmentFixture(t *testing.T) (*Store, model.AgentProfile, model.AgentProfile, model.Task) {
	t.Helper()
	s := roleTestStore(t)
	ctx := context.Background()
	makeAgent := func(cap string) model.AgentProfile {
		r := publishTestRole(t, s, cap)
		a, e := s.CreateAgent(ctx, model.AgentProfile{Name: cap, RoleID: r.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test", MaxConcurrent: 1})
		if e != nil {
			t.Fatal(e)
		}
		return a
	}
	a, b := makeAgent("code.implement"), makeAgent("design.review")
	task, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Implement feature", Goal: "Implement feature and test it", Requirements: model.TaskRequirements{RoleID: a.RoleID}})
	if err != nil {
		t.Fatal(err)
	}
	return s, a, b, task
}
func developmentFinish(t *testing.T, s *Store, r model.Run, seq int64, result workflow.Result) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(context.Background(), model.RuntimeEvent{RuntimeID: r.RuntimeID, Epoch: "epoch-role", RuntimeSeq: seq, RunID: r.ID, TaskID: r.TaskID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
}
func submittedPlan(body string) workflow.Result {
	return workflow.Result{Outcome: "review", Message: body, Artifacts: []workflow.File{{Name: "plan.md", Content: body}}, PlanScope: &workflow.PlanScope{Repository: "oceanbase/seekdb", BaseBranch: "master"}, ValidationPlan: testValidationPlan()}
}

func testValidationPlan() *workflow.ValidationPlan {
	return &workflow.ValidationPlan{
		Reuse: []workflow.ValidationItem{{Scenario: "unchanged parser tests", Reason: "parser is outside the patch", Evidence: "baseline run at approved commit"}},
		Rerun: []workflow.ValidationItem{{Scenario: "changed persistence path", Reason: "implementation changes this path", Evidence: "existing regression must run on candidate"}},
		Add:   []workflow.ValidationItem{}, Exclude: []workflow.ValidationItem{}, FinalGate: []string{"build candidate", "run persistence regression"},
	}
}

func TestDevelopmentRetryPreservesApprovalAndSession(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	first := startWork(t, s, dev, task)
	developmentFinish(t, s, first, 1, submittedPlan("plan"))
	planned := developmentState(t, s, task.ID)
	if planned.ValidationPlan == nil || len(planned.ValidationPlan.Rerun) != 1 || len(planned.ValidationPlan.FinalGate) != 2 {
		t.Fatal("structured validation plan was not persisted", planned.ValidationPlan)
	}
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
	if _, err := s.RetryDevelopment(ctx, task.ID, current.Version-1); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale retry accepted", err)
	}
	if _, err := s.RetryDevelopment(ctx, task.ID, current.Version); err != nil {
		t.Fatal(err)
	}
	after := developmentState(t, s, task.ID)
	if after.Phase != "IMPLEMENTING" || after.ApprovedReviewID != before.ApprovedReviewID || after.PlanHash != before.PlanHash || after.Version != before.Version {
		t.Fatal("retry changed approval", after)
	}
	if _, err := s.RetryDevelopment(ctx, task.ID, current.Version); !errors.Is(err, model.ErrConflict) {
		t.Fatal("duplicate retry accepted", err)
	}
	next := startWork(t, s, dev, task)
	if next.SessionID != first.SessionID || outboxSpec(t, s, next.ID).ExecutionGrant == nil {
		t.Fatal("retry lost session/grant")
	}
	if spec := outboxSpec(t, s, next.ID); !strings.Contains(spec.Instructions, "结构化验证策略") || !strings.Contains(spec.Instructions, "persistence regression") {
		t.Fatal("approved validation strategy missing from implementation instructions")
	}
	if spec := outboxSpec(t, s, next.ID); !strings.Contains(spec.Instructions, "SeekDB 仓库构建策略") || !strings.Contains(spec.Instructions, "ob-make --inc -s") || !strings.Contains(spec.Instructions, "ob-make -C <构建目录>") {
		t.Fatal("SeekDB build guidance missing from implementation instructions")
	}
}

func TestVerificationAmendmentRetainsApprovedProductPlan(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	first := startWork(t, s, dev, task)
	developmentFinish(t, s, first, 1, submittedPlan("approved product plan"))
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	planReview := startWork(t, s, reviewer, child)
	developmentFinish(t, s, planReview, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "product plan is sound", Artifacts: []workflow.File{}})
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", "approved"); err != nil {
		t.Fatal(err)
	}
	implementation := startWork(t, s, dev, task)
	before := *developmentState(t, s, task.ID)
	amended := testValidationPlan()
	amended.Add = []workflow.ValidationItem{{Scenario: "lease contention regression", Reason: "reviewer requires a durable focused test", Evidence: "review finding"}}
	developmentFinish(t, s, implementation, 3, workflow.Result{
		Outcome:               "amend_validation",
		Message:               "Add the requested focused regression without changing product behavior.",
		Artifacts:             []workflow.File{},
		ValidationPlan:        amended,
		VerificationAmendment: &workflow.VerificationAmendment{Reason: "Only repository test coverage is added", Paths: []string{"unittest/logservice/replay_status_test.cpp", "tools/obtest/t/logservice/replay_status.test"}},
		TaskUpdate:            &workflow.TaskUpdate{Kind: "feature", Reason: "Close coverage gap", Approach: "Add focused tests"},
	})
	after := developmentState(t, s, task.ID)
	if after.Phase != "IMPLEMENTING" || after.Version != before.Version || after.PlanRunID != before.PlanRunID || after.PlanHash != before.PlanHash || after.ApprovedReviewID != before.ApprovedReviewID {
		t.Fatalf("verification amendment invalidated product approval: before=%+v after=%+v", before, after)
	}
	if after.ValidationPlan == nil || len(after.ValidationPlan.Add) != 1 || after.ValidationPlan.Add[0].Scenario != "lease contention regression" {
		t.Fatal("amended validation strategy was not retained", after.ValidationPlan)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if current.State != model.TaskStateQueued {
		t.Fatal("verification amendment did not continue implementation", current.State)
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='VerificationAmendmentAccepted'`, task.ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("verification amendment audit missing", events, err)
	}
	next := startWork(t, s, dev, task)
	spec := outboxSpec(t, s, next.ID)
	if next.SessionID != implementation.SessionID || spec.ExecutionGrant == nil || spec.ExecutionGrant.ReviewID != before.ApprovedReviewID || !strings.Contains(spec.Instructions, "lease contention regression") {
		t.Fatal("verification amendment lost approval, session, or updated test strategy", next, spec.ExecutionGrant, spec.Instructions)
	}
}

func TestUserValidationScopeAmendmentPreservesApprovalAndSession(t *testing.T) {
	ctx := context.Background()
	s, developer, task, implementation := approvedImplementation(t)
	before := *developmentState(t, s, task.ID)
	if err := s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: implementation.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: implementation.ID, TaskID: task.ID, Type: "run.interrupted", Error: "context canceled"}); err != nil {
		t.Fatal(err)
	}
	paused, err := s.GetTask(ctx, task.ID)
	if err != nil || paused.State != model.TaskStatePaused {
		t.Fatalf("fixture did not become idle and paused: %+v %v", paused, err)
	}
	amendedPlan := model.DevelopmentValidationPlan{
		Reuse:     []model.DevelopmentValidationItem{{Scenario: "exact four-profile mirror replacement", Reason: "the patch was already applied in the approved worktree", Evidence: "record the candidate diff against BASE_SHA"}},
		Rerun:     []model.DevelopmentValidationItem{{Scenario: "download every dependency URL from the affected profiles", Reason: "the accepted risk is availability of the replacement mirror", Evidence: "use an Android development environment when available; record commands and results"}},
		Exclude:   []model.DevelopmentValidationItem{{Scenario: "macOS ARM init and release build", Reason: "the user explicitly accepted the unavailable ARM environment as a validation limitation", Evidence: "must remain visible in the delivery evidence"}},
		FinalGate: []string{"the exact diff only changes the approved mirror hostnames", "all affected mirrors.oceanbase.com dependency archives are downloadable", "the PR body records the accepted macOS ARM validation limitation"},
	}
	amended, err := s.AmendApprovedValidationPlan(ctx, task.ID, ApprovedValidationPlanAmendment{
		ExpectedVersion: paused.Version,
		Reason:          "No macOS ARM environment is available; validate mirror downloads only and do not reopen plan review.",
		ValidationPlan:  amendedPlan,
		IdempotencyKey:  "scope-amendment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if amended.Phase != "IMPLEMENTING" || amended.Version != before.Version || amended.PlanRunID != before.PlanRunID || amended.PlanHash != before.PlanHash || amended.ApprovedReviewID != before.ApprovedReviewID {
		t.Fatalf("scope amendment invalidated product approval: before=%+v after=%+v", before, amended)
	}
	if amended.ValidationPlan == nil || len(amended.ValidationPlan.Exclude) != 1 || amended.ValidationPlan.Exclude[0].Scenario != "macOS ARM init and release build" {
		t.Fatalf("updated validation plan missing: %+v", amended.ValidationPlan)
	}
	queued, _ := s.GetTask(ctx, task.ID)
	if queued.State != model.TaskStateQueued {
		t.Fatal("scope amendment did not queue implementation", queued.State)
	}
	if _, err = s.AmendApprovedValidationPlan(ctx, task.ID, ApprovedValidationPlanAmendment{ExpectedVersion: paused.Version, Reason: "idempotent replay", ValidationPlan: amendedPlan, IdempotencyKey: "scope-amendment"}); err != nil {
		t.Fatal("idempotent replay should return the accepted amendment", err)
	}
	var events int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='UserValidationScopeAmendmentAccepted'`, task.ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("user validation amendment audit missing", events, err)
	}
	next := startWork(t, s, developer, task)
	spec := outboxSpec(t, s, next.ID)
	if next.SessionID != implementation.SessionID || spec.ExecutionGrant == nil || spec.ExecutionGrant.ReviewID != before.ApprovedReviewID || !strings.Contains(spec.Instructions, "macOS ARM init and release build") || !strings.Contains(spec.Instructions, "用户已受控确认调整验证范围") {
		t.Fatal("scope amendment lost session, grant, or instructions", next, spec.ExecutionGrant, spec.Instructions)
	}
}

func TestUserValidationScopeAmendmentRejectsStaleOrInvalidPlans(t *testing.T) {
	ctx := context.Background()
	s, _, task, implementation := approvedImplementation(t)
	current, _ := s.GetTask(ctx, task.ID)
	valid := model.DevelopmentValidationPlan{Reuse: []model.DevelopmentValidationItem{{Scenario: "existing proof", Reason: "unaffected", Evidence: "baseline"}}, FinalGate: []string{"record evidence"}}
	if _, err := s.AmendApprovedValidationPlan(ctx, task.ID, ApprovedValidationPlanAmendment{ExpectedVersion: current.Version - 1, Reason: "change", ValidationPlan: valid}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale task version accepted scope amendment", err)
	}
	if err := s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: implementation.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: implementation.ID, TaskID: task.ID, Type: "run.interrupted", Error: "context canceled"}); err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetTask(ctx, task.ID)
	if _, err := s.AmendApprovedValidationPlan(ctx, task.ID, ApprovedValidationPlanAmendment{ExpectedVersion: current.Version, Reason: "change", ValidationPlan: model.DevelopmentValidationPlan{}}); !errors.Is(err, model.ErrValidation) {
		t.Fatal("invalid validation plan accepted", err)
	}
}

func TestActiveUserValidationScopePersistsWithoutInterruptOrAgentOverride(t *testing.T) {
	ctx := context.Background()
	s, dev, task, running := approvedImplementation(t)
	before := *developmentState(t, s, task.ID)
	current, _ := s.GetTask(ctx, task.ID)
	narrow := model.DevelopmentValidationPlan{
		Rerun:     []model.DevelopmentValidationItem{{Scenario: "Linux focused test", Reason: "directly affected code"}},
		Exclude:   []model.DevelopmentValidationItem{{Scenario: "Windows matrix", Reason: "user excluded platform validation"}},
		FinalGate: []string{"Linux focused test passes", "record excluded coverage"},
	}
	if _, err := s.AmendApprovedValidationPlan(ctx, task.ID, ApprovedValidationPlanAmendment{ExpectedVersion: current.Version, Reason: "user limits tests to Linux", ValidationPlan: narrow, IdempotencyKey: "active-scope"}); err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetTask(ctx, task.ID)
	stillRunning, _ := s.GetRun(ctx, running.ID)
	var interrupts int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM directive WHERE run_id=?`, running.ID).Scan(&interrupts); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	if current.State != model.TaskStateInProgress || stillRunning.State != running.State || interrupts != 0 || d.Phase != before.Phase || d.Version != before.Version || d.PlanHash != before.PlanHash || d.ApprovedReviewID != before.ApprovedReviewID {
		t.Fatal("active scope update interrupted work or invalidated approval", current, d, stillRunning.State, interrupts)
	}
	developmentFinish(t, s, running, 3, workflow.Result{Outcome: "blocked", Message: "current build finished", Artifacts: []workflow.File{}, RecoveryRequest: &workflow.RecoveryRequest{Evidence: "current build output", NextStep: "continue validation"}})
	next := startWork(t, s, dev, task)
	instructions := outboxSpec(t, s, next.ID).Instructions
	if next.SessionID != running.SessionID || !strings.Contains(instructions, "Linux focused test") || !strings.Contains(instructions, "此范围持续生效") || strings.LastIndex(instructions, "本轮生效的持久化") < strings.LastIndex(instructions, "当前待评审/已批准方案") {
		t.Fatal("new turn did not prioritize the durable scope", instructions)
	}
	broad := testValidationPlan()
	broad.Add = []workflow.ValidationItem{{Scenario: "Windows matrix", Reason: "restore historical tests"}}
	developmentFinish(t, s, next, 4, workflow.Result{Outcome: "amend_validation", Message: "restore old matrix", Artifacts: []workflow.File{}, ValidationPlan: broad, VerificationAmendment: &workflow.VerificationAmendment{Reason: "test coverage", Paths: []string{"tests/matrix_test.cpp"}}, TaskUpdate: &workflow.TaskUpdate{Kind: "feature", Reason: "coverage", Approach: "restore matrix"}})
	d = developmentState(t, s, task.ID)
	if d.ValidationPlan.Rerun[0].Scenario != "Linux focused test" || len(d.ValidationPlan.FinalGate) != 2 || d.ApprovedReviewID != before.ApprovedReviewID {
		t.Fatal("Agent overwrote user validation scope", d)
	}
	var ignored int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='AgentValidationScopeOverrideIgnored'`, task.ID).Scan(&ignored); err != nil || ignored != 1 {
		t.Fatal("Agent override was not audited", ignored, err)
	}
}

func TestImplementationRejectsNonMaterialReplanAndKeepsApprovedPlan(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	first := startWork(t, s, dev, task)
	developmentFinish(t, s, first, 1, submittedPlan("approved product plan"))
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	planReview := startWork(t, s, reviewer, child)
	developmentFinish(t, s, planReview, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "approved", Artifacts: []workflow.File{}})
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if _, err := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", "approved"); err != nil {
		t.Fatal(err)
	}
	implementation := startWork(t, s, dev, task)
	approved := *developmentState(t, s, task.ID)
	legacy := submittedPlan("legacy replan only adds test coverage")
	legacy.Outcome = "replan" // no material plan_change evidence
	developmentFinish(t, s, implementation, 3, legacy)
	current, _ := s.GetTask(ctx, task.ID)
	config, _ := s.GetWorkConfig(ctx, task.ID)
	if d := developmentState(t, s, task.ID); d.Phase != "IMPLEMENTING" || d.PlanRunID != approved.PlanRunID || d.PlanHash != approved.PlanHash || d.ApprovedReviewID != approved.ApprovedReviewID || current.State != model.TaskStateBlocked || !config.Paused || !strings.Contains(config.SchedulerError, "replan requires plan_change") {
		t.Fatalf("non-material replan replaced the approved plan: development=%+v task=%+v config=%+v", d, current, config)
	}
	if _, err := s.RetryWork(ctx, task.ID, current.Version); err != nil {
		t.Fatal(err)
	}
	next := startWork(t, s, dev, task)
	if next.SessionID != implementation.SessionID || outboxSpec(t, s, next.ID).ExecutionGrant == nil || outboxSpec(t, s, next.ID).ExecutionGrant.ReviewID != approved.ApprovedReviewID {
		t.Fatal("recovery lost the original session or approval", next, outboxSpec(t, s, next.ID).ExecutionGrant)
	}
}

func TestRepositoryBuildGuidanceMatchesOnlySeekDB(t *testing.T) {
	for _, repository := range []string{
		"oceanbase/seekdb",
		"https://github.com/oceanbase/seekdb.git",
		"git@github.com:oceanbase/seekdb.git",
		"ssh://git@github.com/oceanbase/seekdb/",
	} {
		if guidance := repositoryBuildGuidance(repository); !strings.Contains(guidance, "ob-make --inc") {
			t.Fatalf("missing guidance for %q", repository)
		}
	}
	for _, repository := range []string{"", "oceanbase/seekdb-bindings", "other/seekdb"} {
		if guidance := repositoryBuildGuidance(repository); guidance != "" {
			t.Fatalf("unexpected guidance for %q: %s", repository, guidance)
		}
	}
}
func developmentState(t *testing.T, s *Store, task string) *model.Development {
	t.Helper()
	w, e := s.GetWorkDetail(context.Background(), task)
	if e != nil || w.Development == nil {
		t.Fatal("missing lifecycle", e)
	}
	return w.Development
}
func outboxSpec(t *testing.T, s *Store, runID string) model.RunSpec {
	t.Helper()
	var raw []byte
	if e := s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE method='run.start' AND json_extract(params_json,'$.run_id')=?`, runID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var spec model.RunSpec
	if e := json.Unmarshal(raw, &spec); e != nil {
		t.Fatal(e)
	}
	return spec
}

func TestPlanningAcceptsCompleteReplanWithKnownPRReference(t *testing.T) {
	ctx := context.Background()
	s, developer, _, task := developmentFixture(t)
	run := startWork(t, s, developer, task)
	source := saveTestSource(t, s, "github")
	const prURL = "https://github.com/oceanbase/seekdb/pull/1405"
	if _, err := s.RegisterPR(ctx, task.ID, source.ID, prURL); err != nil {
		t.Fatal(err)
	}
	revised := submittedPlan("complete revised plan")
	revised.Outcome = "replan"
	revised.PlanChange = &workflow.PlanChange{Kind: "external_contract", ApprovedAssumption: "The linked PR is only a source reference and does not affect the plan.", NewEvidence: "The confirmed source PR changes the public contract required by this task.", AffectedAreas: []string{"public contract", "compatibility tests"}}
	revised.TaskUpdate = &workflow.TaskUpdate{Kind: "feature", Analysis: "The source PR establishes a changed public contract.", Approach: "Revise the implementation and compatibility coverage.", Reason: "The approved contract assumption is false.", Validation: "Re-run the public compatibility gate."}
	revised.PullRequests = []workflow.PullRequest{{URL: prURL}}
	developmentFinish(t, s, run, 1, revised)
	d := developmentState(t, s, task.ID)
	current, _ := s.GetTask(ctx, task.ID)
	config, _ := s.GetWorkConfig(ctx, task.ID)
	if d.Phase != "AGENT_REVIEW" || d.PlanRunID != run.ID || current.State == model.TaskStateBlocked || config.Paused || config.SchedulerError != "" {
		t.Fatalf("complete revised plan was not routed to review: development=%+v task=%+v config=%+v", d, current, config)
	}
}

func TestPlanningRejectsNewPRRegistration(t *testing.T) {
	ctx := context.Background()
	s, developer, _, task := developmentFixture(t)
	run := startWork(t, s, developer, task)
	plan := submittedPlan("plan with an unregistered PR")
	plan.PullRequests = []workflow.PullRequest{{URL: "https://github.com/oceanbase/seekdb/pull/9876"}}
	developmentFinish(t, s, run, 1, plan)
	current, _ := s.GetTask(ctx, task.ID)
	config, _ := s.GetWorkConfig(ctx, task.ID)
	if current.State != model.TaskStateBlocked || !config.Paused || !strings.Contains(config.SchedulerError, "方案阶段不允许") {
		t.Fatalf("new PR side effect was accepted during planning: task=%+v config=%+v", current, config)
	}
}

func TestDevelopmentPlanReviewHumanGateAndSessionContinuity(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	first := startWork(t, s, dev, task)
	if !outboxSpec(t, s, first.ID).ReadOnly {
		t.Fatal("planning writable")
	}
	developmentFinish(t, s, first, 1, submittedPlan("plan one"))
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if len(w.Reviews) != 0 || w.Development.Phase != "AGENT_REVIEW" {
		t.Fatal("premature human approval", w.Development)
	}
	if e := s.RoutePlanReviews(ctx); e != nil {
		t.Fatal(e)
	}
	d := developmentState(t, s, task.ID)
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	reviewOne := startWork(t, s, reviewer, child)
	developmentFinish(t, s, reviewOne, 2, workflow.Result{Outcome: "review", ReviewDecision: "changes_requested", Message: "Add boundary tests", Artifacts: []workflow.File{}})
	if developmentState(t, s, task.ID).Phase != "AGENT_REVIEW" {
		t.Fatal("feedback not delivered")
	}
	if got, _ := s.GetTask(ctx, child.ID); got.State != model.TaskStateCompleted {
		t.Fatal("completed reviewer was left waiting for nonexistent subtasks", got.State)
	}
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, child.ID); got.State != model.TaskStateCompleted {
		t.Fatal("same plan was rerouted before its revision", got.State)
	}
	second := startWork(t, s, dev, task)
	if second.SessionID != first.SessionID {
		t.Fatal("developer lost session")
	}
	developmentFinish(t, s, second, 3, submittedPlan("plan two with boundary tests"))
	if e := s.RoutePlanReviews(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.RoutePlanReviews(ctx); e != nil {
		t.Fatal(e)
	}
	if developmentState(t, s, task.ID).ReviewerTaskID != child.ID {
		t.Fatal("reviewer task replaced")
	}
	if got, _ := s.GetTask(ctx, child.ID); got.State != model.TaskStateQueued {
		t.Fatal("next plan revision did not queue the reused reviewer", got.State)
	}
	reviewTwo := startWork(t, s, reviewer, child)
	if reviewTwo.SessionID != reviewOne.SessionID {
		t.Fatal("reviewer lost session")
	}
	developmentFinish(t, s, reviewTwo, 4, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "Boundaries covered, plan ready for human", Artifacts: []workflow.File{}})
	w, _ = s.GetWorkDetail(ctx, task.ID)
	if w.Development.Phase != "HUMAN_REVIEW" || len(w.Reviews) != 1 || w.Reviews[0].Kind != "plan" || w.Reviews[0].RunID != second.ID {
		t.Fatal("invalid plan approval", w.Development, w.Reviews)
	}
	if got, _ := s.GetTask(ctx, child.ID); got.State != model.TaskStateCompleted {
		t.Fatal("reviewer verdict should complete its own task", got.State)
	}
	if _, e := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "APPROVED", ""); !errors.Is(e, model.ErrValidation) {
		t.Fatal("generic approval bypass", e)
	}
	if _, e := s.StartWorkRun(ctx, CreateRunRequest{TaskID: task.ID, AgentID: dev.ID, RuntimeID: dev.RuntimeID, AdapterID: dev.AdapterID}, workflow.JSONContract{}); !errors.Is(e, model.ErrConflict) {
		t.Fatal("started before approval", e)
	}
	if _, e := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", "approved"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.GetLatestTaskSummary(ctx, task.ID); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("plan approval completed task")
	}
	implementation := startWork(t, s, dev, task)
	spec := outboxSpec(t, s, implementation.ID)
	if spec.ReadOnly || spec.ExecutionGrant == nil || spec.ExecutionGrant.PlanHash != w.Development.PlanHash || implementation.SessionID != first.SessionID {
		t.Fatal("wrong execution grant or session")
	}
	// A premature implementation review is corrected in the original Session;
	// it neither creates human acceptance nor invents a pre-PR review gate.
	developmentFinish(t, s, implementation, 5, workflow.Result{Outcome: "review", Message: "Only wrote a document", Artifacts: []workflow.File{{Name: "report.md", Content: "Not implemented"}}})
	state, _ := s.GetTask(ctx, task.ID)
	if state.State != model.TaskStateQueued {
		t.Fatal("premature delivery was not continued", state.State)
	}
	continued := startWork(t, s, dev, task)
	if continued.SessionID != implementation.SessionID || outboxSpec(t, s, continued.ID).ExecutionGrant == nil {
		t.Fatal("premature delivery lost session or approval")
	}
	if instructions := outboxSpec(t, s, continued.ID).Instructions; !strings.Contains(instructions, "不要安排 PR 前的独立 Agent 复审") || !strings.Contains(instructions, "publish_request") {
		t.Fatal("missing corrective continuation", instructions)
	}
	snapshot := filepath.Join(t.TempDir(), "copy.sqlite")
	if e := s.Backup(ctx, snapshot); e != nil {
		t.Fatal(e)
	}
	restored, e := Open(snapshot)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	copy := developmentState(t, restored, task.ID)
	if copy.ApprovedReviewID == "" || copy.PlanHash != w.Development.PlanHash {
		t.Fatal("approval lost in backup")
	}
}

func TestRoutePlanReviewsReconcilesLegacyIdleReviewerState(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	plan := startWork(t, s, dev, task)
	developmentFinish(t, s, plan, 1, submittedPlan("approved product plan"))
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	d := developmentState(t, s, task.ID)
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	review := startWork(t, s, reviewer, child)
	developmentFinish(t, s, review, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "ready", Artifacts: []workflow.File{}})
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		return setWorkStateTx(ctx, tx, child.ID, model.TaskStateWaiting)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RoutePlanReviews(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(ctx, child.ID); got.State != model.TaskStateCompleted {
		t.Fatal("legacy idle reviewer state was not reconciled", got.State)
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='PlanReviewerIdleStateReconciled'`, child.ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("missing legacy reviewer reconciliation audit event", events, err)
	}
}

func TestDevelopmentStaleReviewerAndRestartKeepHistory(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	r := startWork(t, s, dev, task)
	developmentFinish(t, s, r, 1, submittedPlan("old"))
	if e := s.RoutePlanReviews(ctx); e != nil {
		t.Fatal(e)
	}
	d := developmentState(t, s, task.ID)
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	oldReview := startWork(t, s, reviewer, child)
	if _, e := s.MessageWorkWithMode(ctx, task.ID, "Return to design", "scope", false, WorkMessagePlanChange); e != nil {
		t.Fatal(e)
	}
	developmentFinish(t, s, oldReview, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "old plan passed", Artifacts: []workflow.File{}})
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if len(w.Reviews) != 0 || w.Development.Phase != "PLANNING" {
		t.Fatal("stale reviewer approved new input")
	}
	current, _ := s.GetTask(ctx, task.ID)
	if _, e := s.RestartDevelopment(ctx, task.ID, current.Version-1); !errors.Is(e, model.ErrConflict) {
		t.Fatal("stale reset accepted")
	}
	if _, e := s.RestartDevelopment(ctx, task.ID, current.Version); e != nil {
		t.Fatal(e)
	}
	w, _ = s.GetWorkDetail(ctx, task.ID)
	if len(w.Artifacts) != 1 || w.Development.Phase != "PLANNING" {
		t.Fatal("reset deleted history")
	}
	next := startWork(t, s, dev, task)
	if next.SessionID != r.SessionID {
		t.Fatal("reset discarded original session")
	}
}

func TestDevelopmentNoReviewerDoesNotSkipGate(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	current, _ := s.GetTask(ctx, task.ID)
	if _, e := s.RestartDevelopment(ctx, task.ID, current.Version); e != nil {
		t.Fatal(e)
	}
	run := startWork(t, s, a, task)
	developmentFinish(t, s, run, 1, submittedPlan("needs reviewer"))
	if e := s.RoutePlanReviews(ctx); e != nil {
		t.Fatal(e)
	}
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if w.Development.Phase != "AGENT_REVIEW" || len(w.Reviews) != 0 || w.Config.SchedulerError == "" {
		t.Fatal("missing reviewer silently skipped", w.Development)
	}
}

func TestDevelopmentContinuesIntoPRReviewAndMergedCompletion(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	source := saveTestSource(t, s, "github")
	plan := startWork(t, s, dev, task)
	developmentFinish(t, s, plan, 1, submittedPlan("complete plan"))
	if e := s.RoutePlanReviews(ctx); e != nil {
		t.Fatal(e)
	}
	child, _ := s.GetTask(ctx, developmentState(t, s, task.ID).ReviewerTaskID)
	rr := startWork(t, s, reviewer, child)
	developmentFinish(t, s, rr, 2, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "plan ready", Artifacts: []workflow.File{}})
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if _, e := s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "PLAN_APPROVED", ""); e != nil {
		t.Fatal(e)
	}
	implementation := startWork(t, s, dev, task)
	delivery := workflow.Result{Outcome: "review", Message: "Implemented and tested; PR submitted", Artifacts: []workflow.File{}, PullRequests: []workflow.PullRequest{{URL: "https://github.com/oceanbase/seekdb/pull/123", SourceID: source.ID}}}
	developmentFinish(t, s, implementation, 3, delivery)
	targets, e := s.ListSourceTargets(ctx)
	if e != nil || len(targets) != 1 {
		t.Fatal(targets, e)
	}
	target := targets[0]
	head := strings.Repeat("a", 40)
	pollStore(t, s, source, target, head, model.SourceEvent{Key: "head:" + head, Kind: "github.head", HeadSHA: head, Message: "new commit"})
	reviews, e := s.ListSourceReviews(ctx)
	if e != nil || len(reviews) != 1 {
		t.Fatal(reviews, e)
	}
	prChild, _ := s.GetTask(ctx, reviews[0].TaskID)
	prRun := startWork(t, s, reviewer, prChild)
	if outboxSpec(t, s, prRun.ID).ExecutionGrant != nil {
		t.Fatal("PR reviewer received development write grant")
	}
	developmentFinish(t, s, prRun, 4, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "reviewed latest commit", Artifacts: []workflow.File{}})
	if e = s.CollectSourceReviews(ctx); e != nil {
		t.Fatal(e)
	}
	follow := startWork(t, s, dev, task)
	if follow.SessionID != plan.SessionID {
		t.Fatal("PR feedback lost developer session")
	}
	developmentFinish(t, s, follow, 5, delivery)
	if e = s.ReconcilePublications(ctx); e != nil {
		t.Fatal(e)
	}
	ps, e := s.ListPublications(ctx, task.ID)
	if e != nil || len(ps) != 1 {
		t.Fatal("missing sticky review comment", e, len(ps))
	}
	for _, marker := range []string{
		"## Agent code review · Design reviewer",
		"Reviewer role: **" + reviewer.Role.Name + "** (`" + reviewer.RoleID + "`)",
		"Reviewer member: **" + reviewer.Name + "** (`" + reviewer.ID + "`)",
		"Review task: `" + prChild.ID + "`",
	} {
		if !strings.Contains(ps[0].Body, marker) {
			t.Fatalf("sticky review comment missing identity %q: %s", marker, ps[0].Body)
		}
	}
	claim, e := s.ClaimPublication(ctx, ps[0].Key)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.FinishPublication(ctx, claim, "42", "https://github.com/oceanbase/seekdb/pull/123#issuecomment-42", "SYNCED", ""); e != nil {
		t.Fatal(e)
	}
	w, _ = s.GetWorkDetail(ctx, task.ID)
	final := w.Reviews[0]
	if _, e = s.DecideReview(ctx, task.ID, final.ID, "APPROVED", ""); !errors.Is(e, model.ErrConflict) {
		t.Fatal("accepted before merge", e)
	}
	pollStore(t, s, source, targetByID(t, s, target.ID), head, model.SourceEvent{Key: "merged:" + head, Kind: "github.merged", HeadSHA: head, Message: "merged externally"})
	done, _ := s.GetTask(ctx, task.ID)
	if done.State != model.TaskStateCompleted {
		t.Fatal("not completed")
	}
	w, _ = s.GetWorkDetail(ctx, task.ID)
	for _, review := range w.Reviews {
		if review.ID == final.ID && review.State != "SUPERSEDED" {
			t.Fatal("obsolete final acceptance survived merge", review.State)
		}
	}
	if _, e = s.DecideReview(ctx, task.ID, final.ID, "APPROVED", "accepted after merge"); !errors.Is(e, model.ErrConflict) {
		t.Fatal("merged task accepted an obsolete review", e)
	}
	if _, e = s.GetLatestTaskSummary(ctx, task.ID); e != nil {
		t.Fatal("missing final summary", e)
	}
}

func TestDevelopmentIdenticalPlanStillGetsNewReviewRound(t *testing.T) {
	ctx := context.Background()
	s, dev, reviewer, task := developmentFixture(t)
	first := startWork(t, s, dev, task)
	developmentFinish(t, s, first, 1, submittedPlan("unchanged proposal"))
	if e := s.RoutePlanReviews(ctx); e != nil {
		t.Fatal(e)
	}
	d := developmentState(t, s, task.ID)
	hash := d.PlanHash
	child, _ := s.GetTask(ctx, d.ReviewerTaskID)
	rr := startWork(t, s, reviewer, child)
	if !strings.Contains(outboxSpec(t, s, rr.ID).Instructions, task.Goal) {
		t.Fatal("reviewer lost original acceptance criteria")
	}
	developmentFinish(t, s, rr, 2, workflow.Result{Outcome: "review", ReviewDecision: "changes_requested", Message: "Please reconsider this proposal", Artifacts: []workflow.File{}})
	second := startWork(t, s, dev, task)
	developmentFinish(t, s, second, 3, submittedPlan("unchanged proposal"))
	if developmentState(t, s, task.ID).PlanHash != hash {
		t.Fatal("test must replay identical content")
	}
	if e := s.RoutePlanReviews(ctx); e != nil {
		t.Fatal(e)
	}
	ids, e := s.PendingWork(ctx)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, id := range ids {
		found = found || id == child.ID
	}
	if !found {
		t.Fatal("identical plan revision lost reviewer wakeup")
	}
	next := startWork(t, s, reviewer, child)
	if next.SessionID != rr.SessionID {
		t.Fatal("new review round replaced session")
	}
}
