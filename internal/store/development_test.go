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
	return workflow.Result{Outcome: "review", Message: body, Artifacts: []workflow.File{{Name: "plan.md", Content: body}}, PlanScope: &workflow.PlanScope{Repository: "oceanbase/seekdb", BaseBranch: "master"}}
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
	if developmentState(t, s, task.ID).Phase != "PLANNING" {
		t.Fatal("feedback not delivered")
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
	reviewTwo := startWork(t, s, reviewer, child)
	if reviewTwo.SessionID != reviewOne.SessionID {
		t.Fatal("reviewer lost session")
	}
	developmentFinish(t, s, reviewTwo, 4, workflow.Result{Outcome: "review", ReviewDecision: "passed", Message: "Boundaries covered, plan ready for human", Artifacts: []workflow.File{}})
	w, _ = s.GetWorkDetail(ctx, task.ID)
	if w.Development.Phase != "HUMAN_REVIEW" || len(w.Reviews) != 1 || w.Reviews[0].Kind != "plan" || w.Reviews[0].RunID != second.ID {
		t.Fatal("invalid plan approval", w.Development, w.Reviews)
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
	// Merely returning a document cannot satisfy implementation acceptance.
	developmentFinish(t, s, implementation, 5, workflow.Result{Outcome: "review", Message: "Only wrote a document", Artifacts: []workflow.File{{Name: "report.md", Content: "Not implemented"}}})
	state, _ := s.GetTask(ctx, task.ID)
	if state.State != model.TaskStateBlocked {
		t.Fatal("document accepted as implementation", state.State)
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
	if _, e := s.MessageWork(ctx, task.ID, "Change the scope", "scope", false); e != nil {
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

func TestDevelopmentContinuesIntoPRReviewAndFinalAcceptance(t *testing.T) {
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
	if _, e = s.DecideReview(ctx, task.ID, final.ID, "APPROVED", "accepted after merge"); e != nil {
		t.Fatal(e)
	}
	done, _ := s.GetTask(ctx, task.ID)
	if done.State != model.TaskStateCompleted {
		t.Fatal("not completed")
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
