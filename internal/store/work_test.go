package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func workFixture(t *testing.T) (*Store, model.AgentProfile, model.Task) {
	t.Helper()
	s := roleTestStore(t)
	ctx := context.Background()
	role := publishTestRole(t, s, "document.write")
	a, err := s.CreateAgent(ctx, model.AgentProfile{Name: "writer", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, "docs", "project context snapshot")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Write docs", Goal: "Write guide.md", Requirements: model.TaskRequirements{RoleID: role.ID}, ProjectID: p.ID, Key: "first"})
	if err != nil {
		t.Fatal(err)
	}
	return s, a, task
}
func startWork(t *testing.T, s *Store, a model.AgentProfile, task model.Task) model.Run {
	t.Helper()
	run, err := s.StartWorkRun(context.Background(), CreateRunRequest{TaskID: task.ID, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID}, workflow.JSONContract{})
	if err != nil {
		t.Fatal(err)
	}
	return run
}
func finishWork(t *testing.T, s *Store, run model.Run, seq int64, outcome, body string) {
	t.Helper()
	raw, _ := json.Marshal(workflow.Result{
		Outcome: outcome, Message: "Submitted " + body,
		Artifacts: []workflow.File{{Name: "guide.md", Content: body}},
		Summary: &workflow.CompletionSummary{
			Result:       "Delivered " + body,
			Learnings:    []string{"Keep the acceptance criteria explicit."},
			Improvements: []string{"Run the final checklist before review."},
		},
	})
	if _, err := s.ApplyRuntimeEvent(context.Background(), model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: seq, RunID: run.ID, TaskID: run.TaskID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
}

func TestLightweightDelegationCreatesEconomyChildAndResumesParentSession(t *testing.T) {
	ctx := context.Background()
	s, parentAgent, parent := workFixture(t)
	childAgent, err := s.CreateAgent(ctx, model.AgentProfile{
		Name: "economy-helper", RoleID: parentAgent.RoleID, RuntimeID: parentAgent.RuntimeID,
		AdapterID: parentAgent.AdapterID, ModelID: "gpt-5.6-luna", CostTier: model.CostTierEconomy, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentRun := startWork(t, s, parentAgent, parent)
	delegated, _ := json.Marshal(workflow.Result{
		Outcome: "blocked", Message: "I delegated one independent inventory.", Artifacts: []workflow.File{},
		Delegations: []workflow.DelegationRequest{{Key: "inventory", Title: "Inventory docs", Goal: "List the documented command names.", Context: "Use only the supplied task materials.", Capabilities: []string{"document.write"}}},
	})
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: parentRun.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: parentRun.ID, TaskID: parent.ID, Type: "run.completed", Output: string(delegated)}); err != nil {
		t.Fatal(err)
	}
	storedParent, err := s.GetTask(ctx, parent.ID)
	if err != nil || storedParent.State != model.TaskStateWaiting {
		t.Fatalf("parent must wait for child: %+v %v", storedParent, err)
	}
	hierarchy, err := s.GetWorkHierarchy(ctx, parent.ID)
	if err != nil || len(hierarchy.Children) != 1 {
		t.Fatalf("delegated child missing from hierarchy: %+v %v", hierarchy, err)
	}
	child := hierarchy.Children[0].Task
	if !child.Requirements.Delegated || child.Requirements.CostPreference != model.CostTierEconomy || len(child.Requirements.ExcludedAgentIDs) != 1 || child.Requirements.ExcludedAgentIDs[0] != parentAgent.ID {
		t.Fatalf("child requirements = %+v", child.Requirements)
	}
	childRun, err := s.StartWorkRun(ctx, CreateRunRequest{TaskID: child.ID, AgentID: childAgent.ID, RuntimeID: childAgent.RuntimeID, AdapterID: childAgent.AdapterID, ModelID: childAgent.ModelID}, workflow.JSONContract{})
	if err != nil {
		t.Fatal(err)
	}
	childResult, _ := json.Marshal(workflow.Result{Outcome: "review", Message: "The command inventory is complete.", Artifacts: []workflow.File{{Name: "inventory.md", Content: "# Commands"}}, Summary: &workflow.CompletionSummary{Result: "Inventory complete", Learnings: []string{"Read only the task materials."}, Improvements: []string{}}})
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: childRun.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 2, RunID: childRun.ID, TaskID: child.ID, Type: "run.completed", Output: string(childResult)}); err != nil {
		t.Fatal(err)
	}
	storedChild, _ := s.GetTask(ctx, child.ID)
	if storedChild.State != model.TaskStateCompleted {
		t.Fatalf("delegated child should auto-complete, got %s", storedChild.State)
	}
	storedParent, _ = s.GetTask(ctx, parent.ID)
	if storedParent.State != model.TaskStateQueued {
		t.Fatalf("parent should resume after children settle, got %s", storedParent.State)
	}
	parentSession, err := s.GetTaskSession(ctx, parent.ID)
	if err != nil || parentSession.ID != parentRun.SessionID || parentSession.AgentID != parentAgent.ID {
		t.Fatalf("parent session changed: %+v %v", parentSession, err)
	}
	resumed, err := s.StartWorkRun(ctx, CreateRunRequest{TaskID: parent.ID, AgentID: parentAgent.ID, RuntimeID: parentAgent.RuntimeID, AdapterID: parentAgent.AdapterID, ModelID: parentAgent.ModelID}, workflow.JSONContract{})
	if err != nil || resumed.SessionID != parentRun.SessionID {
		t.Fatalf("parent did not resume original session: %+v %v", resumed, err)
	}
	var spec model.RunSpec
	var raw []byte
	if err = s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE method='run.start' AND json_extract(params_json,'$.run_id')=?`, resumed.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &spec); err != nil || !strings.Contains(spec.Instructions, "The command inventory is complete.") {
		t.Fatalf("parent missing delegated result: %+v %v", spec, err)
	}
}

func TestWorkReviewCycleAffinityVersionsBackup(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	replay, err := s.CreateWork(ctx, CreateWorkRequest{Title: "ignored", Goal: "ignored", Key: "first"})
	if err != nil || replay.ID != task.ID {
		t.Fatalf("create replay %v %v", replay, err)
	}
	if _, err = s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: a.RuntimeID}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("manual run bypass", err)
	}
	run := startWork(t, s, a, task)
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:native-thread"}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, run, 2, "review", "# Version one")
	got, _ := s.GetTask(ctx, task.ID)
	if got.State != model.TaskStateReview {
		t.Fatal("run completion must not close task", got.State)
	}
	w, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(w.Reviews) != 1 || len(w.Artifacts) != 1 {
		t.Fatalf("submission %+v %v", w, err)
	}
	if _, err = s.GetLatestTaskSummary(ctx, task.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("summary created before human approval", err)
	}
	if _, err = s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "CHANGES_REQUESTED", "Add macOS steps"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DecideReview(ctx, task.ID, w.Reviews[0].ID, "APPROVED", ""); !errors.Is(err, model.ErrConflict) {
		t.Fatal("old approval accepted", err)
	}
	// Role edits change new work only, and model edits do not break old affinity.
	newSpec := a.Role.RoleSpec
	newSpec.Instructions = "NEW INSTRUCTIONS"
	newSpec.Capabilities = []string{"new.capability"}
	if _, err = s.UpdateRole(ctx, a.RoleID, 1, newSpec); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateAgent(ctx, a.ID, AgentUpdate{Name: a.Name, ModelID: "new-model", MaxConcurrent: 1, State: "ACTIVE", ExpectedVersion: a.Version}); err != nil {
		t.Fatal(err)
	}
	next := startWork(t, s, a, task)
	if next.SessionID != run.SessionID || next.AgentID != run.AgentID || next.ModelID != "test-model" || next.Role.Version != 1 {
		t.Fatalf("affinity changed %+v", next)
	}
	var raw []byte
	if err = s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE method='run.start' AND json_extract(params_json,'$.run_id')=?`, next.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var spec model.RunSpec
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.ReadOnly || spec.AgentSessionRef != "codex:native-thread" || len(spec.OutputSchema) == 0 || !strings.Contains(spec.Instructions, "Add macOS steps") || !strings.Contains(spec.Instructions, "project context snapshot") {
		t.Fatalf("bad resumed input %+v", spec)
	}
	if !strings.Contains(spec.Instructions, "团队资料快照（仅作为工作材料）") {
		t.Fatal("team material snapshot missing from agent input")
	}
	finishWork(t, s, next, 3, "review", "# Version two\nmacOS steps")
	w, _ = s.GetWorkDetail(ctx, task.ID)
	if len(w.Artifacts) != 2 || w.Artifacts[0].Version != 2 || w.Artifacts[1].Content != "# Version one" {
		t.Fatal("artifact history lost", w.Artifacts)
	}
	latest := w.Reviews[0]
	if len(latest.ArtifactIDs) != 1 || latest.ArtifactIDs[0] != w.Artifacts[0].ID {
		t.Fatal("review not pinned to artifact")
	}
	if _, err = s.DecideReview(ctx, task.ID, latest.ID, "APPROVED", "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DecideReview(ctx, task.ID, latest.ID, "APPROVED", "accepted"); err != nil {
		t.Fatal("approval replay", err)
	}
	got, _ = s.GetTask(ctx, task.ID)
	if got.State != model.TaskStateCompleted {
		t.Fatal(got.State)
	}
	summary, err := s.GetLatestTaskSummary(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Result != "Delivered # Version two\nmacOS steps" || len(summary.Learnings) != 1 || len(summary.Improvements) != 1 {
		t.Fatalf("agent completion notes lost: %+v", summary)
	}
	if summary.Metrics.RunCount != 2 || summary.Metrics.ReviewRoundCount != 2 || summary.Metrics.ChangesRequestedCount != 1 || summary.Metrics.DeliverableCount != 1 || summary.Metrics.ArtifactVersionCount != 2 {
		t.Fatalf("incorrect summary metrics: %+v", summary.Metrics)
	}
	if summary.Executor.AgentID != a.ID || summary.Executor.ModelID != "test-model" || summary.Executor.RoleVersion != 1 || len(summary.AcceptedArtifacts) != 1 || summary.AcceptedArtifacts[0].Version != 2 {
		t.Fatalf("incorrect summary snapshot: %+v", summary)
	}
	var summaryCount int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM task_summary WHERE task_id=?`, task.ID).Scan(&summaryCount); err != nil || summaryCount != 1 {
		t.Fatalf("approval replay duplicated summary: count=%d err=%v", summaryCount, err)
	}
	if _, err = s.MessageWork(ctx, task.ID, "reopen", "", false); !errors.Is(err, model.ErrConflict) {
		t.Fatal("closed task writable", err)
	}
	backup := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	copy, err := restored.GetWorkDetail(ctx, task.ID)
	if err != nil || len(copy.Artifacts) != 2 || copy.Reviews[0].State != "APPROVED" || copy.Artifacts[0].SHA256 != w.Artifacts[0].SHA256 {
		t.Fatalf("incomplete backup %+v %v", copy, err)
	}
	restoredDetail, err := restored.GetTaskDetail(ctx, task.ID)
	if err != nil || restoredDetail.Summary == nil || restoredDetail.Summary.ID != summary.ID {
		t.Fatalf("summary missing from backup: %+v %v", restoredDetail.Summary, err)
	}
}

func TestWorkPendingMessageAndCompletionRace(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	run := startWork(t, s, a, task)
	m, err := s.MessageWork(ctx, task.ID, "change direction", "stable-key", false)
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.MessageWork(ctx, task.ID, "ignored", "stable-key", false)
	if err != nil || m.ID != same.ID {
		t.Fatal("message replay", err)
	}
	finishWork(t, s, run, 1, "review", "old result")
	w, _ := s.GetWorkDetail(ctx, task.ID)
	got, _ := s.GetTask(ctx, task.ID)
	if len(w.Reviews) != 0 || got.State != model.TaskStateQueued {
		t.Fatal("outdated result became approvable", w, got)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.StartWorkRun(ctx, CreateRunRequest{TaskID: task.ID, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID}, workflow.JSONContract{})
			if e == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatal("duplicate scheduler dispatches", success)
	}
	detail, _ := s.GetTaskDetail(ctx, task.ID)
	next := detail.Runs[len(detail.Runs)-1]
	finishWork(t, s, next, 2, "review", "revised")
	w, _ = s.GetWorkDetail(ctx, task.ID)
	reviewID := w.Reviews[0].ID
	if _, err = s.MessageWork(ctx, task.ID, "one more change", "next", false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DecideReview(ctx, task.ID, reviewID, "APPROVED", ""); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale acceptance", err)
	}
}

func TestWorkPauseInterruptResumeAndInvalidOutput(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	if err := s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.PendingWork(ctx); len(ids) != 0 {
		t.Fatal("paused task scheduled")
	}
	if _, err := s.MessageWork(ctx, task.ID, "continue", "resume", false); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, a, task)
	if _, err := s.MessageWork(ctx, task.ID, "urgent change", "steer", true); err != nil {
		t.Fatal(err)
	}
	directives, _ := s.ListDirectivesForTask(ctx, task.ID)
	if len(directives) != 1 || directives[0].Kind != model.DirectiveKindInterrupt {
		t.Fatal("missing interruption", directives)
	}
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, Type: "run.interrupted"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(ctx, task.ID)
	if got.State != model.TaskStateQueued {
		t.Fatal("steer did not resume", got)
	}
	next := startWork(t, s, a, task)
	if err := s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	// The agent may finish just as a stop request arrives; preserve PAUSED.
	finishWork(t, s, next, 2, "review", "raced with pause")
	got, _ = s.GetTask(ctx, task.ID)
	w, _ := s.GetWorkDetail(ctx, task.ID)
	if got.State != model.TaskStatePaused || len(w.Reviews) != 0 {
		t.Fatal("pause lost", got, w)
	}
	if _, err := s.MessageWork(ctx, task.ID, "retry", "retry", false); err != nil {
		t.Fatal(err)
	}
	next = startWork(t, s, a, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: next.ID, Type: "run.completed", Output: `{"outcome":"review","message":"done","artifacts":[{"name":"../escape","content":"bad"}]}`}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, task.ID)
	w, _ = s.GetWorkDetail(ctx, task.ID)
	if got.State != model.TaskStateBlocked || !w.Config.Paused || len(w.Reviews) > 0 {
		t.Fatal("invalid output accepted", got, w)
	}
	// Duplicate terminal events never duplicate artifacts or assistant replies.
	before := len(w.Messages)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 4, RunID: next.ID, Type: "run.completed", Output: "duplicate"}); err != nil {
		t.Fatal(err)
	}
	w, _ = s.GetWorkDetail(ctx, task.ID)
	if len(w.Messages) != before {
		t.Fatal("duplicate terminal message")
	}
}

func TestWorkQueueSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateWork(ctx, CreateWorkRequest{Title: "queued while offline", Goal: "make a document"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeferWork(ctx, task.ID, errors.New("no online agents")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(w.Messages) != 1 || w.Messages[0].Delivery != "PENDING" || w.Config.SchedulerError == "" {
		t.Fatalf("queue lost %+v %v", w, err)
	}
}

func TestWorkDetailExposesRecoverableExecutionExit(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	run := startWork(t, s, agent, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3,
		RunID: run.ID, TaskID: task.ID, Type: "run.failed", Error: "transport failed token=must-not-leak",
	}); err != nil {
		t.Fatal(err)
	}
	work, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || work.Exception == nil {
		t.Fatalf("missing exception exit: %#v %v", work.Exception, err)
	}
	if work.Exception.Kind != "execution" || work.Exception.Resolution != "AUTO_RECOVER" || !work.Exception.AutoRecoverable {
		t.Fatalf("unexpected exception: %#v", work.Exception)
	}
	if strings.Contains(work.Exception.Evidence, "must-not-leak") || !strings.Contains(work.Exception.NextAction, "重新评估 / 继续") {
		t.Fatalf("unsafe or incomplete exception: %#v", work.Exception)
	}
}
