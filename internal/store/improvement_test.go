package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func insertExperience(t *testing.T, s *Store, item model.Experience) {
	t.Helper()
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO experience VALUES(?,?,?,?,?,?,?)`, item.ID, item.Revision, item.State, item.Generation, item.Version, item.UpdatedAtMS, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO experience_revision VALUES(?,?,?,?)`, item.ID, item.Revision, raw, item.CreatedAtMS); err != nil {
		t.Fatal(err)
	}
}

func optimizationWork(t *testing.T) (*Store, model.AgentProfile, model.Task, model.ImprovementScope) {
	t.Helper()
	s := roleTestStore(t)
	ctx := context.Background()
	role := publishTestRole(t, s, "document.write")
	agent, err := s.CreateAgent(ctx, model.AgentProfile{Name: "optimization writer", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Write a guide", Goal: "Deliver a verified guide", TaskType: "document", Repository: "docs/guide", WorkflowType: "standard", Requirements: model.TaskRequirements{RoleID: role.ID}, Key: "optimization-root"})
	if err != nil {
		t.Fatal(err)
	}
	scope := model.ImprovementScope{SourceType: "manual", TaskType: "document", Repository: "docs/guide", RoleID: role.ID, Workflow: "standard", RuntimeOS: "other"}
	return s, agent, task, scope
}

func insertExperimentFixture(t *testing.T, s *Store, source model.Task, scope model.ImprovementScope, name, state string) model.ImprovementExperiment {
	t.Helper()
	now := time.Now().UnixMilli()
	candidate := model.ImprovementCandidate{ID: name + "-candidate", Type: "prompt", Title: name, Rationale: "deterministic test evidence", Scope: scope, Patch: json.RawMessage(`{"overlay":"test"}`), SourceTaskID: source.ID, State: state, Version: 2, ExperimentID: name + "-experiment", CreatedAtMS: now, UpdatedAtMS: now}
	rawCandidate, _ := json.Marshal(candidate)
	if _, err := s.db.Exec(`INSERT INTO improvement_candidate VALUES(?,?,?,?,?,?,?,?,?)`, candidate.ID, candidate.Type, candidate.State, candidate.Version, candidate.SourceTaskID, now, now, name+"-fingerprint", rawCandidate); err != nil {
		t.Fatal(err)
	}
	experiment := model.ImprovementExperiment{ID: candidate.ExperimentID, CandidateID: candidate.ID, Scope: scope, State: state, BaselinePolicyID: defaultPolicyID, BaselinePolicyVersion: 1, CreatedAtMS: now, UpdatedAtMS: now}
	rawExperiment, _ := json.Marshal(experiment)
	if _, err := s.db.Exec(`INSERT INTO improvement_experiment VALUES(?,?,?,?,?,?)`, experiment.ID, candidate.ID, experiment.State, now, now, rawExperiment); err != nil {
		t.Fatal(err)
	}
	return experiment
}

func insertEvaluationFixture(t *testing.T, s *Store, experiment model.ImprovementExperiment, arm string, index int, quality float64, human int, cycle, tokens int64, noProgress int) {
	t.Helper()
	task, _, err := s.CreateTask(context.Background(), fmt.Sprintf("%s-%s-%d", experiment.ID, arm, index), "experiment sample", "evaluate deterministic evidence")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	evaluation := model.TaskEvaluation{ID: fmt.Sprintf("evaluation-%s-%s-%d", experiment.ID, arm, index), TaskID: task.ID, RootTaskID: task.ID, ExperimentID: experiment.ID, Arm: arm, TaskType: "document", Mature: true, Coverage: 100, QualityScore: quality, HumanInterventions: human, CycleTimeMS: cycle, TotalTokens: &tokens, NoProgressRuns: noProgress, CreatedAtMS: now, UpdatedAtMS: now}
	raw, _ := json.Marshal(evaluation)
	if _, err = s.db.Exec(`INSERT INTO task_evaluation VALUES(?,?,?,?,?,?,?)`, task.ID, task.ID, experiment.ID, arm, 1, now, raw); err != nil {
		t.Fatal(err)
	}
}

func TestTaskOptimizationFreezesExperienceForSessionAndChildren(t *testing.T) {
	ctx := context.Background()
	s, agent, task, scope := optimizationWork(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 7; i++ {
		action := fmt.Sprintf("recommended action %d", i)
		if i == 0 {
			action = strings.Repeat("large but valid experience ", 400)
		}
		insertExperience(t, s, model.Experience{ID: fmt.Sprintf("experience-%d", i), Revision: 1, Version: 1, Generation: int64(i + 1), State: "ACTIVE", Situation: "matching task", Action: action, Avoid: "guessing", Verification: "check evidence", Scope: scope, EffectScore: float64(100 - i), CreatedAtMS: now, UpdatedAtMS: now})
	}
	run := startWork(t, s, agent, task)
	optimization, err := s.GetTaskOptimization(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if optimization.Assignment.ExperienceGeneration != 7 || len(optimization.SessionExperiences) != 5 {
		t.Fatalf("unexpected frozen catalog/session recall: %#v", optimization)
	}
	var firstRaw []byte
	if err = s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE json_extract(params_json,'$.run_id')=?`, run.ID).Scan(&firstRaw); err != nil {
		t.Fatal(err)
	}
	var firstSpec model.RunSpec
	if err = json.Unmarshal(firstRaw, &firstSpec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstSpec.Instructions, "本 Session 固定注入") || strings.Contains(firstSpec.Instructions, "large but valid experience") {
		t.Fatalf("experience budget was not enforced: %s", firstSpec.Instructions)
	}

	insertExperience(t, s, model.Experience{ID: "experience-new", Revision: 1, Version: 1, Generation: 8, State: "ACTIVE", Situation: "matching task", Action: "NEW EXPERIENCE MUST NOT ENTER THE ACTIVE SESSION", Verification: "test", Scope: scope, EffectScore: 1000, CreatedAtMS: now + 1, UpdatedAtMS: now + 1})
	finishWork(t, s, run, 1, "needs_input", "draft")
	if _, err = s.MessageWork(ctx, task.ID, "continue", "optimization-continue", false); err != nil {
		t.Fatal(err)
	}
	next := startWork(t, s, agent, task)
	if next.SessionID != run.SessionID {
		t.Fatal("continuation changed the Agent session")
	}
	var nextRaw []byte
	if err = s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE json_extract(params_json,'$.run_id')=?`, next.ID).Scan(&nextRaw); err != nil {
		t.Fatal(err)
	}
	var nextSpec model.RunSpec
	if err = json.Unmarshal(nextRaw, &nextSpec); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(nextSpec.Instructions, "NEW EXPERIENCE MUST NOT ENTER") {
		t.Fatal("a published experience changed an existing session")
	}

	child, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Child", Goal: "Review one section", TaskType: "review", Requirements: model.TaskRequirements{RoleID: agent.RoleID}, Key: "optimization-child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms) VALUES('optimization-edge',?,?,?,?)`, task.ID, child.ID, model.TaskEdgeDecomposedInto, now); err != nil {
		t.Fatal(err)
	}
	childAssignment, _, err := s.PrepareTaskOptimization(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if childAssignment.RootTaskID != task.ID || childAssignment.PolicyID != optimization.Assignment.PolicyID || childAssignment.ExperienceGeneration != optimization.Assignment.ExperienceGeneration || childAssignment.Arm != optimization.Assignment.Arm {
		t.Fatalf("child did not inherit root assignment: %#v", childAssignment)
	}
}

func TestTaskProfileFinalizesRuntimeBeforeFirstRunOnly(t *testing.T) {
	ctx := context.Background()
	s, _, task, scope := optimizationWork(t)
	if err := s.RegisterRuntime(ctx, model.RuntimeHello{RuntimeID: "linux-runtime", Epoch: "linux-epoch", OS: "linux", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}}}}); err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreateAgent(ctx, model.AgentProfile{Name: "linux writer", RoleID: scope.RoleID, RuntimeID: "linux-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope.RuntimeOS = "linux"
	now := time.Now().UnixMilli()
	insertExperience(t, s, model.Experience{ID: "linux-experience", Revision: 1, Version: 1, Generation: 1, State: "ACTIVE", Situation: "linux work", Action: "use linux evidence", Verification: "verify on linux", Scope: scope, CreatedAtMS: now, UpdatedAtMS: now})
	prepared, _, err := s.PrepareTaskOptimization(ctx, task.ID)
	if err != nil || prepared.Profile.RuntimeOS != "other" {
		t.Fatalf("pre-routing profile should preserve unknown OS: %#v, %v", prepared, err)
	}
	run := startWork(t, s, agent, task)
	optimization, err := s.GetTaskOptimization(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if optimization.Assignment.Profile.RuntimeOS != "linux" || len(optimization.SessionExperiences) != 1 || optimization.SessionExperiences[0].ExperienceID != "linux-experience" {
		t.Fatalf("first run did not finalize profile before recall: %#v", optimization)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	afterRun, _, err := ensureOptimizationAssignmentTx(ctx, tx, task.ID, CreateRunRequest{TaskID: task.ID, AgentID: agent.ID, RuntimeID: "role-runtime"})
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if afterRun.Profile.RuntimeOS != "linux" || afterRun.RootTaskID != run.TaskID {
		t.Fatalf("existing run profile was repinned: %#v", afterRun)
	}
}

func TestExistingTaskDoesNotEnterExperimentStartedLater(t *testing.T) {
	ctx := context.Background()
	s, _, existing, scope := optimizationWork(t)
	var observationStarted int64
	if err := s.db.QueryRow(`SELECT CAST(value AS INTEGER) FROM improvement_meta WHERE key='observation_started_at_ms'`).Scan(&observationStarted); err != nil {
		t.Fatal(err)
	}
	experiment := insertExperimentFixture(t, s, existing, scope, "prospective-only", model.CandidateStateCanary)
	experiment.CreatedAtMS = observationStarted + 100
	experiment.UpdatedAtMS = experiment.CreatedAtMS
	rawExperiment, _ := json.Marshal(experiment)
	if _, err := s.db.Exec(`UPDATE improvement_experiment SET created_at_ms=?,updated_at_ms=?,data_json=? WHERE experiment_id=?`, experiment.CreatedAtMS, experiment.UpdatedAtMS, rawExperiment, experiment.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE task SET created_at_ms=? WHERE task_id=?`, observationStarted+50, existing.ID); err != nil {
		t.Fatal(err)
	}
	oldAssignment, _, err := s.PrepareTaskOptimization(ctx, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldAssignment.ExperimentID != "" || oldAssignment.Arm != model.ExperimentArmBaseline {
		t.Fatalf("task created before experiment entered prospective canary: %#v", oldAssignment)
	}
	for time.Now().UnixMilli() < experiment.CreatedAtMS {
		time.Sleep(time.Millisecond)
	}
	newTask, err := s.CreateWork(ctx, CreateWorkRequest{Title: "New guide", Goal: "Deliver a verified guide", TaskType: "document", Repository: "docs/guide", WorkflowType: "standard", Requirements: model.TaskRequirements{RoleID: scope.RoleID}, Key: "prospective-new-root"})
	if err != nil {
		t.Fatal(err)
	}
	newAssignment, _, err := s.PrepareTaskOptimization(ctx, newTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newAssignment.ExperimentID != experiment.ID {
		t.Fatalf("task created after experiment was not assigned prospectively: %#v", newAssignment)
	}
}

func recallForTest(t *testing.T, s *Store, assignment model.OptimizationAssignment) []model.Experience {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	items, _, err := recallExperiencesTx(context.Background(), tx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func insertExperienceCandidate(t *testing.T, s *Store, source model.Task, scope model.ImprovementScope, patch model.ExperiencePatch, name string) model.ImprovementExperiment {
	t.Helper()
	now := time.Now().UnixMilli()
	rawPatch, _ := json.Marshal(patch)
	candidate := model.ImprovementCandidate{ID: name + "-candidate", Type: "experience", Title: name, Rationale: "deterministic evidence", Scope: scope, Patch: rawPatch, SourceTaskID: source.ID, State: model.CandidateStateValidated, Version: 1, CreatedAtMS: now, UpdatedAtMS: now}
	rawCandidate, _ := json.Marshal(candidate)
	if _, err := s.db.Exec(`INSERT INTO improvement_candidate VALUES(?,?,?,?,?,?,?,?,?)`, candidate.ID, candidate.Type, candidate.State, candidate.Version, candidate.SourceTaskID, now, now, name+"-fingerprint", rawCandidate); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err == nil {
		candidate, err = activateCandidateTx(context.Background(), tx, candidate)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	experiment, err := s.GetImprovementExperiment(context.Background(), candidate.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	return experiment
}

func assignmentForExperienceTest(task model.Task, scope model.ImprovementScope, experiment model.ImprovementExperiment, arm string, generation int64) model.OptimizationAssignment {
	return model.OptimizationAssignment{RootTaskID: task.ID, Profile: model.TaskProfile{RootTaskID: task.ID, SourceType: scope.SourceType, TaskType: scope.TaskType, Repository: scope.Repository, RoleID: scope.RoleID, Workflow: scope.Workflow, RuntimeOS: scope.RuntimeOS}, ExperimentID: experiment.ID, Arm: arm, ExperienceGeneration: generation}
}

func TestExperienceRevisionCanaryAndRollbackKeepPinnedCatalogs(t *testing.T) {
	s, _, source, scope := optimizationWork(t)
	now := time.Now().UnixMilli()
	base := model.Experience{ID: "revision-target", Revision: 1, Version: 1, Generation: 1, State: "ACTIVE", Situation: "build repository", Action: "old action", Verification: "old check", Scope: scope, CreatedAtMS: now, UpdatedAtMS: now}
	insertExperience(t, s, base)
	experiment := insertExperienceCandidate(t, s, source, scope, model.ExperiencePatch{Operation: "revise", ExperienceID: base.ID, ExpectedRevision: 1, Situation: "build repository", Action: "new supported action", Avoid: "raw flags", Verification: "new check"}, "revision")
	if experiment.ExperienceOperation != "revise" || experiment.CandidateExperienceRev != 2 || experiment.BaselineExperienceRev != 1 {
		t.Fatalf("revision was not staged immutably: %#v", experiment)
	}
	baseline := assignmentForExperienceTest(source, scope, experiment, model.ExperimentArmBaseline, 2)
	candidate := assignmentForExperienceTest(source, scope, experiment, model.ExperimentArmCandidate, 2)
	if items := recallForTest(t, s, baseline); len(items) != 1 || items[0].Revision != 1 || items[0].Action != "old action" {
		t.Fatalf("baseline saw staged revision: %#v", items)
	}
	if items := recallForTest(t, s, candidate); len(items) != 1 || items[0].Revision != 2 || items[0].Action != "new supported action" {
		t.Fatalf("candidate did not see staged revision: %#v", items)
	}
	tx, _ := s.db.BeginTx(context.Background(), nil)
	if err := transitionExperimentTx(context.Background(), tx, &experiment, model.CandidateStatePromoted, "test promotion"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Existing assignments remain frozen even after publication.
	if items := recallForTest(t, s, baseline); len(items) != 1 || items[0].Action != "old action" {
		t.Fatalf("promotion changed a pinned baseline: %#v", items)
	}
	if items := recallForTest(t, s, candidate); len(items) != 1 || items[0].Action != "new supported action" {
		t.Fatalf("promotion changed a pinned candidate: %#v", items)
	}
	promoted := assignmentForExperienceTest(source, scope, experiment, model.ExperimentArmPromoted, 3)
	if items := recallForTest(t, s, promoted); len(items) != 1 || items[0].Action != "new supported action" || items[0].State != "ACTIVE" {
		t.Fatalf("new assignment missed promoted revision: %#v", items)
	}
	tx, _ = s.db.BeginTx(context.Background(), nil)
	if err := transitionExperimentTx(context.Background(), tx, &experiment, model.CandidateStateRolledBack, "test rollback"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rolledBack := assignmentForExperienceTest(source, scope, experiment, model.ExperimentArmBaseline, 4)
	if items := recallForTest(t, s, rolledBack); len(items) != 1 || items[0].Action != "old action" || items[0].State != "ACTIVE" {
		t.Fatalf("rollback did not restore baseline content: %#v", items)
	}
}

func TestExperienceRetirementIsCanariedAndReversible(t *testing.T) {
	s, _, source, scope := optimizationWork(t)
	now := time.Now().UnixMilli()
	base := model.Experience{ID: "retirement-target", Revision: 1, Version: 1, Generation: 1, State: "ACTIVE", Situation: "obsolete advice", Action: "old action", Verification: "old check", Scope: scope, CreatedAtMS: now, UpdatedAtMS: now}
	insertExperience(t, s, base)
	experiment := insertExperienceCandidate(t, s, source, scope, model.ExperiencePatch{Operation: "retire", ExperienceID: base.ID, ExpectedRevision: 1}, "retirement")
	if items := recallForTest(t, s, assignmentForExperienceTest(source, scope, experiment, model.ExperimentArmBaseline, 1)); len(items) != 1 {
		t.Fatalf("baseline lost retirement target: %#v", items)
	}
	if items := recallForTest(t, s, assignmentForExperienceTest(source, scope, experiment, model.ExperimentArmCandidate, 1)); len(items) != 0 {
		t.Fatalf("candidate still received retired experience: %#v", items)
	}
	tx, _ := s.db.BeginTx(context.Background(), nil)
	if err := transitionExperimentTx(context.Background(), tx, &experiment, model.CandidateStatePromoted, "test retirement"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if items := recallForTest(t, s, assignmentForExperienceTest(source, scope, experiment, model.ExperimentArmPromoted, 2)); len(items) != 0 {
		t.Fatalf("promoted retirement still recalled target: %#v", items)
	}
	tx, _ = s.db.BeginTx(context.Background(), nil)
	if err := transitionExperimentTx(context.Background(), tx, &experiment, model.CandidateStateRolledBack, "restore retirement"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if items := recallForTest(t, s, assignmentForExperienceTest(source, scope, experiment, model.ExperimentArmBaseline, 3)); len(items) != 1 || items[0].Action != "old action" {
		t.Fatalf("retirement rollback did not restore experience: %#v", items)
	}
}

func TestStabilizedExperienceRecordsRealizedEffect(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	experiment := insertExperienceCandidate(t, s, source, scope, model.ExperiencePatch{
		Operation:    "add",
		Situation:    "repository build",
		Action:       "use the repository supported build entrypoint",
		Avoid:        "guessing raw compiler flags",
		Verification: "run the supported build and focused test",
	}, "stable-effect")
	for i := 0; i < 5; i++ {
		insertEvaluationFixture(t, s, experiment, model.ExperimentArmBaseline, i, 70, 5, 10_000, 1000, 3)
		insertEvaluationFixture(t, s, experiment, model.ExperimentArmCandidate, i, 95, 1, 3_000, 400, 0)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		err = evaluateExperimentTx(ctx, tx, experiment.ID)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	experiment, err = s.GetImprovementExperiment(ctx, experiment.ID)
	if err != nil || experiment.State != model.CandidateStatePromoted {
		t.Fatalf("experience did not promote: %#v, %v", experiment, err)
	}
	for i := 0; i < 10; i++ {
		insertEvaluationFixture(t, s, experiment, model.ExperimentArmPromoted, i, 96, 1, 2_500, 350, 0)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err == nil {
		err = evaluateExperimentTx(ctx, tx, experiment.ID)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	experiment, err = s.GetImprovementExperiment(ctx, experiment.ID)
	if err != nil || experiment.State != model.ExperimentStateStable {
		t.Fatalf("promoted observation did not stabilize: %#v, %v", experiment, err)
	}
	experience, err := readJSONRow[model.Experience](s.db.QueryRow(`SELECT data_json FROM experience WHERE experience_id=?`, experiment.CandidateExperienceID))
	if err != nil {
		t.Fatal(err)
	}
	if experience.SampleCount != 15 || experience.EffectScore != experiment.Score.Composite || experience.EffectScore <= 0 {
		t.Fatalf("catalog effect does not reflect canary plus promoted samples: experience=%#v experiment=%#v", experience, experiment)
	}
	var events int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_type='experience' AND aggregate_id=? AND event_type='ExperienceEffectStabilized'`, experience.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("stabilized effect was not auditable: %d, %v", events, err)
	}
}

func TestStableCanarySplitAndDurableSingleWorkerLease(t *testing.T) {
	candidateCount := 0
	for i := 0; i < 1000; i++ {
		first := stableCandidateArm(fmt.Sprintf("task-%d", i), "experiment")
		if first != stableCandidateArm(fmt.Sprintf("task-%d", i), "experiment") {
			t.Fatal("assignment was not stable")
		}
		if first {
			candidateCount++
		}
	}
	if candidateCount < 450 || candidateCount > 550 {
		t.Fatalf("split is not approximately 50/50: %d/1000", candidateCount)
	}

	ctx := context.Background()
	s, _, task, _ := optimizationWork(t)
	if _, _, err := s.PrepareTaskOptimization(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := projectObservationTx(ctx, tx, task.ID, "test")
	if err == nil {
		err = enqueueImprovementJobTx(ctx, tx, "ANALYZE", task.ID, "lease-test", time.Now().UnixMilli(), observation)
	}
	if err == nil {
		err = enqueueImprovementJobTx(ctx, tx, "ANALYZE", task.ID, "lease-test", time.Now().UnixMilli(), observation)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM improvement_job WHERE idempotency_key='lease-test'`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("job idempotency failed: %d, %v", jobs, err)
	}
	if _, err = s.db.Exec(`UPDATE task SET state=? WHERE task_id=?`, model.TaskStateCompleted, task.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimImprovementJob(ctx, "worker-a", "ANALYZE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimImprovementJob(ctx, "worker-b", "ANALYZE"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second analyst ran concurrently: %v", err)
	}
	if err = s.ReleaseImprovementJob(ctx, claimed.ID, "worker-a", errors.New("agent offline")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE improvement_job SET available_at_ms=0,data_json=json_set(data_json,'$.available_at_ms',0)`); err != nil {
		t.Fatal(err)
	}
	claimedAgain, err := s.ClaimImprovementJob(ctx, "worker-after-restart", "ANALYZE")
	if err != nil || claimedAgain.ID != claimed.ID || claimedAgain.Attempts != 1 {
		t.Fatalf("durable job did not resume: %#v, %v", claimedAgain, err)
	}
}

func TestAutomaticallySourcedTaskFinalizesCodeProfileBeforeFirstRun(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	role := publishTestRole(t, s, "code.implement")
	agent, err := s.CreateAgent(ctx, model.AgentProfile{Name: "source developer", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Imported requirement", Goal: "implement the imported requirement", Source: "antmultica", Requirements: model.TaskRequirements{}, Key: "source-profile"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	scope := model.ImprovementScope{SourceType: "antmultica", TaskType: "code", Repository: "other", RoleID: role.ID, Workflow: "standard", RuntimeOS: "other"}
	insertExperience(t, s, model.Experience{ID: "source-code-experience", Revision: 1, Version: 1, Generation: 1, State: "ACTIVE", Situation: "imported code work", Action: "use the supported build workflow", Verification: "run supported validation", Scope: scope, CreatedAtMS: now, UpdatedAtMS: now})

	prepared, _, err := s.PrepareTaskOptimization(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Profile.TaskType != "other" || prepared.Profile.RoleID != "other" {
		t.Fatalf("pre-routing profile guessed facts that the source did not supply: %#v", prepared.Profile)
	}

	startWork(t, s, agent, task)
	optimization, err := s.GetTaskOptimization(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if optimization.Assignment.Profile.TaskType != "code" || optimization.Assignment.Profile.RoleID != role.ID {
		t.Fatalf("selected code workflow was not pinned before the first run: %#v", optimization.Assignment.Profile)
	}
	if len(optimization.SessionExperiences) != 1 || optimization.SessionExperiences[0].ExperienceID != "source-code-experience" {
		t.Fatalf("code-scoped experience was not recalled after profile finalization: %#v", optimization.SessionExperiences)
	}
}

func TestChildSessionRecallsItsRoleFromFrozenRootCatalog(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	developerRole := publishTestRole(t, s, "code.implement")
	reviewerRole := publishTestRole(t, s, "code.review")
	developer, err := s.CreateAgent(ctx, model.AgentProfile{Name: "catalog developer", RoleID: developerRole.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := s.CreateAgent(ctx, model.AgentProfile{Name: "catalog reviewer", RoleID: reviewerRole.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Code work", Goal: "implement and review", TaskType: "code", Repository: "oceanbase/seekdb", WorkflowType: "standard", Requirements: model.TaskRequirements{RoleID: developerRole.ID}, Key: "role-catalog-root"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	baseScope := model.ImprovementScope{SourceType: "manual", TaskType: "code", Repository: "oceanbase/seekdb", Workflow: "standard", RuntimeOS: "other"}
	developerScope, reviewerScope := baseScope, baseScope
	developerScope.RoleID, reviewerScope.RoleID = developerRole.ID, reviewerRole.ID
	insertExperience(t, s, model.Experience{ID: "developer-catalog-experience", Revision: 1, Version: 1, Generation: 1, State: "ACTIVE", Situation: "developer work", Action: "implement with evidence", Verification: "run tests", Scope: developerScope, CreatedAtMS: now, UpdatedAtMS: now})
	insertExperience(t, s, model.Experience{ID: "reviewer-catalog-experience", Revision: 1, Version: 1, Generation: 2, State: "ACTIVE", Situation: "reviewer work", Action: "review the current commit", Verification: "record findings", Scope: reviewerScope, CreatedAtMS: now, UpdatedAtMS: now})

	startWork(t, s, developer, root)
	rootOptimization, err := s.GetTaskOptimization(ctx, root.ID)
	if err != nil || len(rootOptimization.SessionExperiences) != 1 || rootOptimization.SessionExperiences[0].ExperienceID != "developer-catalog-experience" {
		t.Fatalf("root session did not receive developer experience: %#v, %v", rootOptimization, err)
	}
	child, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Review current commit", Goal: "review", TaskType: "review", Requirements: model.TaskRequirements{RoleID: reviewerRole.ID}, Key: "role-catalog-child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms) VALUES('role-catalog-edge',?,?,?,?)`, root.ID, child.ID, model.TaskEdgeReviews, now); err != nil {
		t.Fatal(err)
	}
	startWork(t, s, reviewer, child)
	childOptimization, err := s.GetTaskOptimization(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if childOptimization.Assignment.RootTaskID != root.ID || childOptimization.Assignment.ExperienceGeneration != rootOptimization.Assignment.ExperienceGeneration || childOptimization.Assignment.Arm != rootOptimization.Assignment.Arm {
		t.Fatalf("child did not inherit the frozen root experiment/catalog: %#v", childOptimization.Assignment)
	}
	if len(childOptimization.SessionExperiences) != 1 || childOptimization.SessionExperiences[0].ExperienceID != "reviewer-catalog-experience" {
		t.Fatalf("child session did not select experience for its actual role: %#v", childOptimization.SessionExperiences)
	}
}

func TestPromptOverlayMatchesActualRoleAndRunStage(t *testing.T) {
	policy := defaultOptimizationPolicy(time.Now().UnixMilli())
	policy.Prompt = model.PromptPolicy{RoleID: "reviewer", Stage: "review", Overlay: "REVIEW-ONLY-OVERLAY"}
	if got := optimizationInstructions(policy, "developer", "review"); strings.Contains(got, "REVIEW-ONLY-OVERLAY") {
		t.Fatal("role-scoped prompt leaked to another role")
	}
	if got := optimizationInstructions(policy, "reviewer", "planning"); strings.Contains(got, "REVIEW-ONLY-OVERLAY") {
		t.Fatal("stage-scoped prompt leaked to another stage")
	}
	if got := optimizationInstructions(policy, "reviewer", "review"); !strings.Contains(got, "REVIEW-ONLY-OVERLAY") {
		t.Fatal("matching role/stage prompt was not applied")
	}
}

func TestManualAgentTaskDoesNotEnterRoutingExperiment(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	role := publishTestRole(t, s, "document.write")
	agent, err := s.CreateAgent(ctx, model.AgentProfile{Name: "explicit writer", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	manual, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Explicit assignment", Goal: "write", TaskType: "document", Repository: "docs/guide", WorkflowType: "standard", AgentID: agent.ID, Requirements: model.TaskRequirements{RoleID: role.ID}, Key: "manual-routing-exclusion"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	scope := model.ImprovementScope{SourceType: "manual", TaskType: "document", Repository: "docs/guide", RoleID: role.ID, Workflow: "standard", RuntimeOS: "other"}
	candidate := model.ImprovementCandidate{ID: "routing-candidate", Type: "routing", Title: "route by measured cost", Rationale: "test evidence", Scope: scope, Patch: json.RawMessage(`{"load_weight":40,"success_weight":30,"cost_weight":30}`), Evidence: []model.ExperienceEvidence{{TaskID: manual.ID, Signal: "queue_bottleneck"}}, SourceTaskID: manual.ID, State: model.CandidateStateCanary, Version: 2, ExperimentID: "routing-experiment", CreatedAtMS: now, UpdatedAtMS: now}
	candidateRaw, _ := json.Marshal(candidate)
	if _, err = s.db.Exec(`INSERT INTO improvement_candidate VALUES(?,?,?,?,?,?,?,?,?)`, candidate.ID, candidate.Type, candidate.State, candidate.Version, candidate.SourceTaskID, now, now, "routing-candidate-fingerprint", candidateRaw); err != nil {
		t.Fatal(err)
	}
	policy := defaultOptimizationPolicy(now)
	policy.ID, policy.State, policy.Scope = "routing-candidate-policy", "CANARY", scope
	policy.Routing = model.RoutingPolicy{LoadWeight: 40, SuccessWeight: 30, CostWeight: 30}
	policyRaw, _ := json.Marshal(policy)
	if _, err = s.db.Exec(`INSERT INTO optimization_policy VALUES(?,?,?,?,?)`, policy.ID, policy.Version, policy.State, now, policyRaw); err != nil {
		t.Fatal(err)
	}
	experiment := model.ImprovementExperiment{ID: candidate.ExperimentID, CandidateID: candidate.ID, Scope: scope, State: model.CandidateStateCanary, BaselinePolicyID: defaultPolicyID, BaselinePolicyVersion: 1, CandidatePolicyID: policy.ID, CandidatePolicyVersion: policy.Version, CreatedAtMS: now, UpdatedAtMS: now}
	experimentRaw, _ := json.Marshal(experiment)
	if _, err = s.db.Exec(`INSERT INTO improvement_experiment VALUES(?,?,?,?,?,?)`, experiment.ID, candidate.ID, experiment.State, now, now, experimentRaw); err != nil {
		t.Fatal(err)
	}
	manualAssignment, _, err := s.PrepareTaskOptimization(ctx, manual.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !manualAssignment.Profile.ManualAgent || manualAssignment.ExperimentID != "" || manualAssignment.PolicyID != defaultPolicyID {
		t.Fatalf("manual assignment entered routing experiment: %#v", manualAssignment)
	}
	// Simulate an administrative state change after experiment creation. The
	// active catalog head may move, but neither experiment arm may move away
	// from its captured baseline/candidate version.
	drift := defaultOptimizationPolicy(now + 1)
	drift.ID, drift.Scope = "later-active-policy", scope
	drift.Prompt.Overlay = "must not leak into an existing experiment"
	driftRaw, _ := json.Marshal(drift)
	if _, err = s.db.Exec(`INSERT INTO optimization_policy VALUES(?,?,?,?,?)`, drift.ID, drift.Version, drift.State, drift.CreatedAtMS, driftRaw); err != nil {
		t.Fatal(err)
	}
	automatic, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Automatic assignment", Goal: "write", TaskType: "document", Repository: "docs/guide", WorkflowType: "standard", Requirements: model.TaskRequirements{RoleID: role.ID}, Key: "automatic-routing-control"})
	if err != nil {
		t.Fatal(err)
	}
	autoAssignment, _, err := s.PrepareTaskOptimization(ctx, automatic.ID)
	if err != nil || autoAssignment.ExperimentID != experiment.ID {
		t.Fatalf("routing experiment did not remain available to automatic work: %#v, %v", autoAssignment, err)
	}
	wantPolicy := defaultPolicyID
	if autoAssignment.Arm == model.ExperimentArmCandidate {
		wantPolicy = policy.ID
	}
	if autoAssignment.PolicyID != wantPolicy || autoAssignment.PolicyID == drift.ID {
		t.Fatalf("experiment policy snapshot drifted after assignment: got %#v, want %s", autoAssignment, wantPolicy)
	}
}

func TestImprovementReadinessRequiresObservationWindowAndTelemetryCoverage(t *testing.T) {
	ctx := context.Background()
	s, agent, task, _ := optimizationWork(t)
	run := startWork(t, s, agent, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.progress", Usage: &model.TokenUsage{Provider: "codex", InputTokens: 100, OutputTokens: 20}}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, run, 2, "review", "telemetry-complete guide")
	work, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(work.Reviews) != 1 {
		t.Fatalf("missing review: %#v, %v", work, err)
	}
	if _, err = s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "APPROVED", "accepted"); err != nil {
		t.Fatal(err)
	}
	readiness, err := s.ImprovementReadiness(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Ready || readiness.DataCoverage != 100 || !strings.Contains(readiness.Reason, "24") {
		t.Fatalf("fresh observation incorrectly started experiments: %#v", readiness)
	}
	started := time.Now().Add(-25 * time.Hour).UnixMilli()
	if _, err = s.db.Exec(`UPDATE improvement_meta SET value=? WHERE key='observation_started_at_ms'`, fmt.Sprint(started)); err != nil {
		t.Fatal(err)
	}
	readiness, err = s.ImprovementReadiness(ctx)
	if err != nil || !readiness.Ready || readiness.DataCoverage != 100 || readiness.ObservationHours < 24 {
		t.Fatalf("complete 25-hour observation was not ready: %#v, %v", readiness, err)
	}
	legacy, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Historical completion", Goal: "must not dilute the new observation window", TaskType: "document", Repository: "docs/guide", WorkflowType: "standard", Requirements: model.TaskRequirements{RoleID: agent.RoleID}, Key: "readiness-historical"})
	if err != nil {
		t.Fatal(err)
	}
	legacyRun := startWork(t, s, agent, legacy)
	finishWork(t, s, legacyRun, 3, "review", "historical guide without usage")
	legacyWork, err := s.GetWorkDetail(ctx, legacy.ID)
	if err != nil || len(legacyWork.Reviews) != 1 {
		t.Fatalf("missing historical review: %#v, %v", legacyWork, err)
	}
	if _, err = s.DecideReview(ctx, legacy.ID, legacyWork.Reviews[0].ID, "APPROVED", "historical acceptance"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE task_summary SET completed_at_ms=?,data_json=json_set(data_json,'$.completed_at_ms',?) WHERE task_id=?`, started-1, started-1, legacy.ID); err != nil {
		t.Fatal(err)
	}
	readiness, err = s.ImprovementReadiness(ctx)
	if err != nil || !readiness.Ready || readiness.DataCoverage != 100 {
		t.Fatalf("historical task diluted post-activation coverage: %#v, %v", readiness, err)
	}
	if _, err = s.db.Exec(`DELETE FROM run_token_usage WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	readiness, err = s.ImprovementReadiness(ctx)
	if err != nil || readiness.Ready || readiness.DataCoverage != 0 || !strings.Contains(readiness.Reason, "80") {
		t.Fatalf("missing token telemetry did not close the experiment gate: %#v, %v", readiness, err)
	}
}

func TestImprovementOverviewUsesExperimentSamplesForGrossAndKeepsMissingTokensUnknown(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	experiment := insertExperimentFixture(t, s, source, scope, "token-saving", model.CandidateStateCanary)
	insertEvaluationFixture(t, s, experiment, model.ExperimentArmBaseline, 0, 80, 2, 10_000, 100, 1)
	insertEvaluationFixture(t, s, experiment, model.ExperimentArmBaseline, 1, 80, 2, 10_000, 200, 1)
	insertEvaluationFixture(t, s, experiment, model.ExperimentArmCandidate, 0, 85, 1, 8_000, 50, 0)
	insertEvaluationFixture(t, s, experiment, model.ExperimentArmCandidate, 1, 85, 1, 8_000, 70, 0)
	overview, err := s.ImprovementOverview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if overview.GrossTokenSaving == nil || *overview.GrossTokenSaving != 180 || overview.NetTokenSaving == nil || *overview.NetTokenSaving != 180 {
		t.Fatalf("gross/net savings did not use experiment medians: %#v", overview)
	}
	var evaluation model.TaskEvaluation
	if evaluation, err = readJSONRow[model.TaskEvaluation](s.db.QueryRow(`SELECT data_json FROM task_evaluation WHERE experiment_id=? AND arm=? LIMIT 1`, experiment.ID, model.ExperimentArmCandidate)); err != nil {
		t.Fatal(err)
	}
	evaluation.TotalTokens = nil
	raw, _ := json.Marshal(evaluation)
	if _, err = s.db.Exec(`UPDATE task_evaluation SET data_json=? WHERE task_id=?`, raw, evaluation.TaskID); err != nil {
		t.Fatal(err)
	}
	overview, err = s.ImprovementOverview(ctx)
	if err != nil || overview.GrossTokenSaving != nil || overview.NetTokenSaving != nil {
		t.Fatalf("missing sample token telemetry was reported as zero: %#v, %v", overview, err)
	}
}

func TestHumanAcceptanceReworkDoesNotAnalyzeActiveTask(t *testing.T) {
	ctx := context.Background()
	s, agent, task, _ := optimizationWork(t)
	run := startWork(t, s, agent, task)
	finishWork(t, s, run, 1, "review", "draft guide")
	work, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(work.Reviews) != 1 {
		t.Fatalf("missing review: %#v, %v", work, err)
	}
	if _, err = s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "CHANGES_REQUESTED", "clarify the recovery steps"); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetTask(ctx, task.ID)
	if err != nil || stored.State != model.TaskStateQueued {
		t.Fatalf("improvement observation changed business rework flow: %#v, %v", stored, err)
	}
	var jobs int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM improvement_job WHERE root_task_id=?`, task.ID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("active human rework must not queue an improvement job: jobs=%d err=%v", jobs, err)
	}
}

func TestRepeatedBlockFailureUsesBlockReasonNotRepeatedCommands(t *testing.T) {
	ctx := context.Background()
	s, agent, task, scope := optimizationWork(t)
	experiment := insertExperimentFixture(t, s, task, scope, "block-fingerprint", model.CandidateStateCanary)
	assignment, _, err := s.PrepareTaskOptimization(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	assignment.ExperimentID, assignment.Arm = experiment.ID, model.ExperimentArmCandidate
	assignmentRaw, _ := json.Marshal(assignment)
	if _, err = s.db.Exec(`UPDATE experiment_assignment SET experiment_id=?,arm=?,data_json=? WHERE root_task_id=?`, experiment.ID, assignment.Arm, assignmentRaw, task.ID); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	for i := 0; i < 3; i++ {
		activity := model.Activity{Action: model.Action{ID: fmt.Sprintf("same-command-%d", i), Kind: "command", State: "COMPLETED", Command: "./build.sh"}, TaskID: task.ID, RunID: run.ID, SessionID: run.SessionID, AgentID: agent.ID, RuntimeID: run.RuntimeID, FirstSeq: int64(i + 1), LastSeq: int64(i + 1), StartedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
		raw, _ := json.Marshal(activity)
		if _, err = s.db.Exec(`INSERT INTO run_activity VALUES(?,?,?,?,?,?)`, run.ID, activity.ID, task.ID, activity.FirstSeq, activity.LastSeq, raw); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := projectObservationTx(ctx, tx, task.ID, "test")
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	repeatedCommand, repeatedFailure := false, false
	for _, signal := range observation.Signals {
		repeatedCommand = repeatedCommand || signal.Code == "repeated_command"
		repeatedFailure = repeatedFailure || signal.Code == "repeated_failure"
	}
	if !repeatedCommand || repeatedFailure {
		t.Fatalf("command repetition was misclassified: %#v", observation.Signals)
	}
	blockOnce := func() {
		t.Helper()
		tx, txErr := s.db.BeginTx(ctx, nil)
		if txErr == nil {
			_, txErr = tx.Exec(`UPDATE task_workflow SET scheduler_error='compiler executable is unavailable' WHERE task_id=?`, task.ID)
		}
		if txErr == nil {
			txErr = setWorkStateTx(ctx, tx, task.ID, model.TaskStateBlocked)
		}
		if txErr == nil {
			txErr = tx.Commit()
		} else {
			tx.Rollback()
		}
		if txErr != nil {
			t.Fatal(txErr)
		}
	}
	blockOnce()
	var evaluations int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM task_evaluation WHERE task_id=?`, task.ID).Scan(&evaluations); err != nil || evaluations != 0 {
		t.Fatalf("repeated command manufactured a failed sample: count=%d err=%v", evaluations, err)
	}
	blockOnce()
	blockOnce()
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM task_evaluation WHERE task_id=?`, task.ID).Scan(&evaluations); err != nil || evaluations != 0 {
		t.Fatalf("active repeated blocks must not become an experiment sample: count=%d err=%v", evaluations, err)
	}
	var occurrences int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_type='task' AND aggregate_id=? AND event_type='TaskBlockedObservation'`, task.ID).Scan(&occurrences); err != nil || occurrences != 3 {
		t.Fatalf("block observations are not auditable: count=%d err=%v", occurrences, err)
	}
}

func TestObservationAggregatesQueueAndHumanRedirectionAcrossTaskTree(t *testing.T) {
	ctx := context.Background()
	s, _, root, _ := optimizationWork(t)
	if _, _, err := s.PrepareTaskOptimization(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	child, _, err := s.CreateTask(ctx, "observation-child", "Child task", "produce one part")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms) VALUES('observation-tree-edge',?,?,?,?)`, root.ID, child.ID, model.TaskEdgeDecomposedInto, now); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"initial request", "change the direction"} {
		if _, err = insertMessageTx(ctx, tx, child.ID, "user", content, "", "RECORDED"); err != nil {
			t.Fatal(err)
		}
	}
	childSummary := model.TaskSummary{ID: "observation-child-summary", TaskID: child.ID, Version: 1, SourceType: "task", SourceID: child.ID, Title: child.Title, Goal: child.Goal, Metrics: model.TaskSummaryMetrics{QueueWaitMS: 65_000, RuntimeQueueWaitMS: 5_000, ActiveRunTimeMS: 1_000}, CompletedAtMS: now, CreatedAtMS: now}
	rawSummary, _ := json.Marshal(childSummary)
	if _, err = tx.Exec(`INSERT INTO task_summary(summary_id,task_id,version,source_type,source_id,completed_at_ms,data_json) VALUES(?,?,?,?,?,?,?)`, childSummary.ID, child.ID, childSummary.Version, childSummary.SourceType, childSummary.SourceID, now, rawSummary); err != nil {
		t.Fatal(err)
	}
	observation, err := projectObservationTx(ctx, tx, root.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	queue, redirected := false, false
	for _, signal := range observation.Signals {
		queue = queue || signal.Code == "queue_bottleneck"
		redirected = redirected || signal.Code == "human_redirection"
	}
	if !queue || !redirected {
		t.Fatalf("task-tree diagnostics were incomplete: %#v", observation.Signals)
	}
}

func TestCompletionProjectionReconcilesIdempotentlyAfterRestartGap(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	task, _, err := s.CreateTask(ctx, "reconcile-completion", "Completed root", "persist result and analyze it")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: "role-runtime", AdapterID: "exec-agent", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.completed", Output: "durable result"}); err != nil {
		t.Fatal(err)
	}
	var summaryID string
	if err = s.db.QueryRow(`SELECT summary_id FROM task_summary WHERE task_id=?`, task.ID).Scan(&summaryID); err != nil {
		t.Fatal(err)
	}
	// Simulate a process boundary where business completion committed but the
	// low-priority projection rows were unavailable/lost before reconciliation.
	if _, err = s.db.Exec(`DELETE FROM improvement_job WHERE root_task_id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileImprovementJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileImprovementJobs(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM improvement_job WHERE root_task_id=? AND idempotency_key IN (?,?)`, task.ID, "analyze:"+summaryID, "judge:"+summaryID).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("completion reconciliation was not complete/idempotent: jobs=%d err=%v", jobs, err)
	}
	stored, err := s.GetTask(ctx, task.ID)
	if err != nil || stored.State != model.TaskStateCompleted {
		t.Fatalf("improvement reconciliation changed business completion: %#v %v", stored, err)
	}
}

func TestBlindJudgeReceivesAcceptedContentButNoExperimentArm(t *testing.T) {
	ctx := context.Background()
	s, agent, task, _ := optimizationWork(t)
	run := startWork(t, s, agent, task)
	finishWork(t, s, run, 1, "review", "accepted guide content")
	work, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(work.Reviews) != 1 {
		t.Fatalf("missing review: %#v %v", work, err)
	}
	if _, err = s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "APPROVED", "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE improvement_job SET available_at_ms=0 WHERE root_task_id=? AND kind='JUDGE'`, task.ID); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimImprovementJob(ctx, "blind-judge-test", "JUDGE")
	if err != nil {
		t.Fatal(err)
	}
	if job.Evaluation == nil || len(job.Evaluation.Artifacts) != 1 || job.Evaluation.Artifacts[0].Content != "accepted guide content" || job.Evaluation.Artifacts[0].SHA256 == "" {
		t.Fatalf("judge did not receive immutable accepted content: %#v", job.Evaluation)
	}
	raw, _ := json.Marshal(job.Evaluation)
	if strings.Contains(string(raw), `"arm"`) || strings.Contains(string(raw), `"experiment_id"`) {
		t.Fatalf("blind judge input leaked experiment assignment: %s", raw)
	}
}

func TestEvaluationRecordsLatestObservationDeadline(t *testing.T) {
	ctx := context.Background()
	s, agent, task, _ := optimizationWork(t)
	run := startWork(t, s, agent, task)
	finishWork(t, s, run, 1, "review", "accepted guide content")
	work, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(work.Reviews) != 1 {
		t.Fatalf("missing review: %#v %v", work, err)
	}
	if _, err = s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "APPROVED", "accepted"); err != nil {
		t.Fatal(err)
	}
	var originalDue int64
	if err = s.db.QueryRow(`SELECT available_at_ms FROM improvement_job WHERE root_task_id=? AND kind='JUDGE'`, task.ID).Scan(&originalDue); err != nil {
		t.Fatal(err)
	}
	laterDue := originalDue + time.Hour.Milliseconds()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = enqueueImprovementJobTx(ctx, tx, "JUDGE", task.ID, "later-observation", laterDue, nil); err != nil {
		t.Fatal(err)
	}
	evaluation, _, err := prepareEvaluationTx(ctx, tx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.ObservationDueAtMS != laterDue || evaluation.Mature {
		t.Fatalf("latest observation deadline was not preserved: %#v", evaluation)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestRootEvaluationAggregatesBusinessChildren(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	role := publishTestRole(t, s, "code.implement")
	agent, err := s.CreateAgent(ctx, model.AgentProfile{Name: "tree developer", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Implement feature", Goal: "ship a reviewed PR", TaskType: "code", Repository: "oceanbase/seekdb", WorkflowType: "standard", Requirements: model.TaskRequirements{RoleID: role.ID}, Key: "tree-evaluation-root"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PrepareTaskOptimization(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	child, _, err := s.CreateTask(ctx, "tree-code-child", "Code child", "implement one part")
	if err != nil {
		t.Fatal(err)
	}
	reviewChild, _, err := s.CreateTask(ctx, "tree-review-child", "Review child", "review the current commit")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err = s.db.Exec(`INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms) VALUES('tree-code-edge',?,?,?,?),('tree-review-edge',?,?,?,?)`, root.ID, child.ID, model.TaskEdgeDecomposedInto, now, root.ID, reviewChild.ID, model.TaskEdgeReviews, now); err != nil {
		t.Fatal(err)
	}
	childRun, err := s.CreateRun(ctx, CreateRunRequest{TaskID: child.ID, AgentID: agent.ID, RuntimeID: agent.RuntimeID, AdapterID: agent.AdapterID, ModelID: agent.ModelID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: childRun.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 501, RunID: childRun.ID, TaskID: child.ID, Type: "run.completed", Output: ""}); err != nil {
		t.Fatal(err)
	}
	reviewRun, err := s.CreateRun(ctx, CreateRunRequest{TaskID: reviewChild.ID, AgentID: agent.ID, RuntimeID: agent.RuntimeID, AdapterID: agent.AdapterID, ModelID: agent.ModelID})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, content := range []string{"initial child input", "human correction"} {
		if _, err = insertMessageTx(ctx, tx, child.ID, "user", content, "", "RECORDED"); err != nil {
			t.Fatal(err)
		}
	}
	review := model.Review{ID: "tree-change-review", TaskID: child.ID, RunID: childRun.ID, State: "CHANGES_REQUESTED", Comment: "fix one issue", CreatedAtMS: now, DecidedAtMS: now}
	reviewRaw, _ := json.Marshal(review)
	if _, err = tx.Exec(`INSERT INTO review(review_id,task_id,run_id,state,data_json) VALUES(?,?,?,?,?)`, review.ID, child.ID, childRun.ID, review.State, reviewRaw); err != nil {
		t.Fatal(err)
	}
	source := model.TaskSource{ID: "tree-github", Kind: "github", Name: "evaluation fixture", Enabled: true, Version: 1, IntervalSeconds: 5}
	sourceRaw, _ := json.Marshal(source)
	if _, err = tx.Exec(`INSERT INTO task_source(source_id,data_json) VALUES(?,?)`, source.ID, sourceRaw); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	target := model.SourceTarget{ID: "tree-pr-target", SourceID: source.ID, Entity: "https://github.com/oceanbase/seekdb/pull/1", TaskID: child.ID, Enabled: true, HeadSHA: head, Cursor: json.RawMessage(`{}`), CreatedAtMS: now}
	if err = insertSourceTargetTx(ctx, tx, target); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO source_review(target_id,head_sha,role_id,task_id,state,feedback_sent) VALUES(?,?,?,?,?,1)`, target.ID, head, role.ID, reviewChild.ID, "COMPLETED"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE run SET state='COMPLETED',output=? WHERE run_id=?`, `{"review_decision":"passed"}`, reviewRun.ID); err != nil {
		t.Fatal(err)
	}
	pipeline := model.TestPipeline{ID: "tree-pipeline", TaskID: child.ID, RequestedByTaskID: reviewChild.ID, PRTargetID: target.ID, PRURL: target.Entity, HeadSHA: head, Kind: model.SeekDBTestKind, Attempt: 1, State: "success", PipelineID: 1001, CreatedAtMS: now, UpdatedAtMS: now}
	pipelineRaw, _ := json.Marshal(pipeline)
	if _, err = tx.Exec(`INSERT INTO test_pipeline(request_id,task_id,pr_target_id,head_sha,attempt,state,next_attempt_ms,data_json) VALUES(?,?,?,?,?,?,0,?)`, pipeline.ID, pipeline.TaskID, pipeline.PRTargetID, pipeline.HeadSHA, pipeline.Attempt, pipeline.State, pipelineRaw); err != nil {
		t.Fatal(err)
	}
	merged := model.SourceEvent{ID: "tree-merged", SourceID: source.ID, TargetID: target.ID, Key: "merged:" + head, Kind: "github.merged", TaskID: child.ID, HeadSHA: head, State: "RECORDED", CreatedAtMS: now}
	mergedRaw, _ := json.Marshal(merged)
	if _, err = tx.Exec(`INSERT INTO source_event(event_id,source_id,target_id,state,data_json) VALUES(?,?,?,?,?)`, merged.ID, merged.SourceID, merged.TargetID, merged.State, mergedRaw); err != nil {
		t.Fatal(err)
	}
	summary := model.TaskSummary{ID: "tree-summary", TaskID: root.ID, Version: 1, SourceType: "review", SourceID: "human-review", Title: root.Title, Goal: root.Goal, Result: "delivered", Metrics: model.TaskSummaryMetrics{CycleTimeMS: 10_000}, AcceptanceComment: "accepted", CompletedAtMS: now, CreatedAtMS: now}
	summaryRaw, _ := json.Marshal(summary)
	if _, err = tx.Exec(`UPDATE task SET state='COMPLETED',updated_at_ms=? WHERE task_id=?`, now, root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO task_summary(summary_id,task_id,version,source_type,source_id,completed_at_ms,data_json) VALUES(?,?,?,?,?,?,?)`, summary.ID, root.ID, summary.Version, summary.SourceType, summary.SourceID, now, summaryRaw); err != nil {
		t.Fatal(err)
	}
	if _, err = appendEventTx(ctx, tx, "task", child.ID, "ExecutionPermissionDecided", "permission-review", root.ID, map[string]any{"state": "APPROVED"}); err != nil {
		t.Fatal(err)
	}
	evaluation, _, err := prepareEvaluationTx(ctx, tx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.HumanInterventions != 3 || evaluation.NoProgressRuns != 1 {
		t.Fatalf("child efficiency facts were lost: %#v", evaluation)
	}
	if evaluation.Coverage != 80 || evaluation.QualityScore != 80 || !evaluation.Mature {
		t.Fatalf("child PR/review/pipeline evidence was not attributed to root: %#v", evaluation)
	}
	if _, err = appendEventTx(ctx, tx, "task", root.ID, "ExternalTaskClosed", "source-lifecycle", root.ID, map[string]any{"source": "antmultica"}); err != nil {
		t.Fatal(err)
	}
	closed, _, err := prepareEvaluationTx(ctx, tx, root.ID)
	if err != nil || closed.Coverage != 95 || closed.QualityScore != 95 {
		t.Fatalf("latest source terminal state was not scored: %#v, %v", closed, err)
	}
	if _, err = appendEventTx(ctx, tx, "task", root.ID, "ExternalTaskReopened", "source-lifecycle", root.ID, map[string]any{"source": "antmultica"}); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := prepareEvaluationTx(ctx, tx, root.ID)
	if err != nil || reopened.Coverage != 95 || reopened.QualityScore != 80 {
		t.Fatalf("a reopened source task retained obsolete terminal credit: %#v, %v", reopened, err)
	}
	if _, err = tx.Exec(`UPDATE run SET output=? WHERE run_id=?`, `{"review_decision":"changes_requested"}`, reviewRun.ID); err != nil {
		t.Fatal(err)
	}
	requestedChanges, _, err := prepareEvaluationTx(ctx, tx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requestedChanges.Coverage != 95 || requestedChanges.QualityScore != 55 {
		t.Fatalf("a completed-but-failing reviewer verdict was scored as approval: %#v", requestedChanges)
	}
}

func TestImprovementRunRejectsInvalidAgentOutputWithoutBlockingBusinessTask(t *testing.T) {
	ctx := context.Background()
	s, agent, task, _ := optimizationWork(t)
	if _, _, err := s.PrepareTaskOptimization(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := projectObservationTx(ctx, tx, task.ID, "test")
	if err == nil {
		err = enqueueImprovementJobTx(ctx, tx, "ANALYZE", task.ID, "invalid-output", time.Now().UnixMilli(), observation)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE task SET state=? WHERE task_id=?`, model.TaskStateCompleted, task.ID); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimImprovementJob(ctx, "worker", "ANALYZE")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartImprovementJob(ctx, job.ID, "worker", CreateRunRequest{AgentID: agent.ID, RuntimeID: agent.RuntimeID, AdapterID: agent.AdapterID, ModelID: agent.ModelID}, "analyze", json.RawMessage(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, TaskID: run.TaskID, Type: "run.completed", Output: `not JSON`}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM improvement_job WHERE job_id=?`, job.ID).Scan(&state); err != nil || state != "FAILED" {
		t.Fatalf("invalid output was not isolated: %s, %v", state, err)
	}
	business, err := s.GetTask(ctx, task.ID)
	if err != nil || business.State == model.TaskStateBlocked {
		t.Fatalf("improvement failure blocked business task: %#v, %v", business, err)
	}
	overview, err := s.ImprovementOverview(ctx)
	if err != nil || overview.FailedJobs != 1 || len(overview.NeedsAttention) == 0 {
		t.Fatalf("failure is not visible: %#v, %v", overview, err)
	}
}

func TestFailedCandidateStopsAfterThreeMatureSamplesPerArm(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	now := time.Now().UnixMilli()
	candidate := model.ImprovementCandidate{ID: "bad-candidate", Type: "prompt", Title: "bad", Rationale: "test", Scope: scope, Patch: json.RawMessage(`{"overlay":"bad"}`), SourceTaskID: source.ID, State: model.CandidateStateCanary, Version: 2, ExperimentID: "bad-experiment", CreatedAtMS: now, UpdatedAtMS: now}
	rawCandidate, _ := json.Marshal(candidate)
	if _, err := s.db.Exec(`INSERT INTO improvement_candidate VALUES(?,?,?,?,?,?,?,?,?)`, candidate.ID, candidate.Type, candidate.State, candidate.Version, candidate.SourceTaskID, now, now, "bad-fingerprint", rawCandidate); err != nil {
		t.Fatal(err)
	}
	experiment := model.ImprovementExperiment{ID: candidate.ExperimentID, CandidateID: candidate.ID, Scope: scope, State: model.CandidateStateCanary, BaselinePolicyID: defaultPolicyID, BaselinePolicyVersion: 1, CreatedAtMS: now, UpdatedAtMS: now}
	rawExperiment, _ := json.Marshal(experiment)
	if _, err := s.db.Exec(`INSERT INTO improvement_experiment VALUES(?,?,?,?,?,?)`, experiment.ID, candidate.ID, experiment.State, now, now, rawExperiment); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		task, _, err := s.CreateTask(ctx, fmt.Sprintf("evaluation-%d", i), fmt.Sprintf("sample %d", i), "goal")
		if err != nil {
			t.Fatal(err)
		}
		arm, quality := model.ExperimentArmBaseline, 100.0
		if i >= 3 {
			arm, quality = model.ExperimentArmCandidate, 0
		}
		evaluation := model.TaskEvaluation{ID: fmt.Sprintf("evaluation-%d", i), TaskID: task.ID, RootTaskID: task.ID, ExperimentID: experiment.ID, Arm: arm, TaskType: "document", Mature: true, Coverage: 100, QualityScore: quality, CycleTimeMS: 1000, CreatedAtMS: now, UpdatedAtMS: now}
		raw, _ := json.Marshal(evaluation)
		if _, err = s.db.Exec(`INSERT INTO task_evaluation VALUES(?,?,?,?,?,?,?)`, task.ID, task.ID, experiment.ID, arm, 1, now, raw); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		err = evaluateExperimentTx(ctx, tx, experiment.ID)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetImprovementCandidate(ctx, candidate.ID)
	if err != nil || got.State != model.CandidateStateRejected || !strings.Contains(got.Rationale, "test") {
		t.Fatalf("bad candidate did not stop: %#v, %v", got, err)
	}
}

func TestBlockedChildIsRecordedForFinalRetrospectiveOnly(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	experiment := insertExperimentFixture(t, s, source, scope, "blocked-child", model.CandidateStateCanary)
	experiment.CreatedAtMS = time.Now().Add(-time.Second).UnixMilli()
	experiment.UpdatedAtMS = experiment.CreatedAtMS
	rawExperiment, _ := json.Marshal(experiment)
	if _, err := s.db.Exec(`UPDATE improvement_experiment SET created_at_ms=?,updated_at_ms=?,data_json=? WHERE experiment_id=?`, experiment.CreatedAtMS, experiment.UpdatedAtMS, rawExperiment, experiment.ID); err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Blocked tree root", Goal: "finish all children", TaskType: "document", Repository: "docs/guide", WorkflowType: "standard", Requirements: model.TaskRequirements{RoleID: scope.RoleID}, Key: "blocked-child-root"})
	if err != nil {
		t.Fatal(err)
	}
	assignment, _, err := s.PrepareTaskOptimization(ctx, root.ID)
	if err != nil || assignment.ExperimentID != experiment.ID {
		t.Fatalf("new root was not assigned to experiment: %#v, %v", assignment, err)
	}
	child, _, err := s.CreateTask(ctx, "blocked-child-task", "Blocked child", "repair environment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms) VALUES('blocked-child-edge',?,?,?,?)`, root.ID, child.ID, model.TaskEdgeDecomposedInto, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = setWorkStateTx(ctx, tx, child.ID, model.TaskStateBlocked); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var observations, jobs, evaluations int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_type='task' AND aggregate_id=? AND event_type='TaskBlockedObservation'`, root.ID).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM improvement_job WHERE root_task_id=?`, root.ID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM task_evaluation WHERE task_id=?`, root.ID).Scan(&evaluations); err != nil {
		t.Fatal(err)
	}
	if observations != 1 || jobs != 0 || evaluations != 0 {
		t.Fatalf("active block must remain observation-only: observations=%d jobs=%d evaluations=%d", observations, jobs, evaluations)
	}
}

func TestCanaryRejectedAtThirtyDayDeadlineWithoutNewEvaluation(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	experiment := insertExperimentFixture(t, s, source, scope, "deadline", model.CandidateStateCanary)
	experiment.CreatedAtMS = time.Now().Add(-31 * 24 * time.Hour).UnixMilli()
	experiment.UpdatedAtMS = experiment.CreatedAtMS
	rawExperiment, _ := json.Marshal(experiment)
	if _, err := s.db.Exec(`UPDATE improvement_experiment SET created_at_ms=?,updated_at_ms=?,data_json=? WHERE experiment_id=?`, experiment.CreatedAtMS, experiment.UpdatedAtMS, rawExperiment, experiment.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EvaluateExperimentDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetImprovementExperiment(ctx, experiment.ID)
	if err != nil || got.State != model.CandidateStateRejected || !strings.Contains(got.DecisionReason, "30 天") {
		t.Fatalf("expired canary remained active: %#v, %v", got, err)
	}
}

func TestCandidateAutoPromotesThenRollsBackAfterRegression(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	experiment := insertExperimentFixture(t, s, source, scope, "promotion", model.CandidateStateCanary)
	for i := 0; i < 5; i++ {
		insertEvaluationFixture(t, s, experiment, model.ExperimentArmBaseline, i, 70, 6, 10_000, 1000, 3)
		insertEvaluationFixture(t, s, experiment, model.ExperimentArmCandidate, i, 95, 1, 3_000, 400, 0)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		err = evaluateExperimentTx(ctx, tx, experiment.ID)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetImprovementExperiment(ctx, experiment.ID)
	if err != nil || got.State != model.CandidateStatePromoted || got.Score.Composite < 8 {
		t.Fatalf("candidate did not promote: %#v, %v", got, err)
	}
	for i := 0; i < 10; i++ {
		insertEvaluationFixture(t, s, experiment, model.ExperimentArmPromoted, i, 20, 20, 30_000, 3000, 12)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err == nil {
		err = evaluateExperimentTx(ctx, tx, experiment.ID)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetImprovementExperiment(ctx, experiment.ID)
	if err != nil || got.State != model.CandidateStateRolledBack || !strings.Contains(got.DecisionReason, "-8") {
		t.Fatalf("promoted regression did not roll back: %#v, %v", got, err)
	}
}

func TestSafetyKernelViolationImmediatelyRollsBackCandidate(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	experiment := insertExperimentFixture(t, s, source, scope, "safety", model.CandidateStateCanary)
	assignment, _, err := s.PrepareTaskOptimization(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	assignment.ExperimentID, assignment.Arm = experiment.ID, model.ExperimentArmCandidate
	raw, _ := json.Marshal(assignment)
	if _, err = s.db.Exec(`UPDATE experiment_assignment SET experiment_id=?,arm=?,data_json=? WHERE root_task_id=?`, experiment.ID, assignment.Arm, raw, source.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordImprovementSafetyViolation(ctx, source.ID, model.ImprovementSafetyUnauthorizedExternalIO, "publication guard rejected unapproved target token=not-persisted", "guard-event-1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetImprovementExperiment(ctx, experiment.ID)
	if err != nil || got.State != model.CandidateStateRolledBack {
		t.Fatalf("safety incident did not immediately roll back: %#v, %v", got, err)
	}
	evaluation, err := s.GetTaskEvaluation(ctx, source.ID)
	if err != nil || !evaluation.SafetyViolation || !evaluation.FailedSample || !evaluation.Mature || evaluation.QualityScore != 0 {
		t.Fatalf("safety sample was not preserved: %#v, %v", evaluation, err)
	}
	encoded, _ := json.Marshal(evaluation)
	if strings.Contains(string(encoded), "not-persisted") {
		t.Fatal("safety evidence persisted a secret")
	}
	version := got.UpdatedAtMS
	if err = s.RecordImprovementSafetyViolation(ctx, source.ID, model.ImprovementSafetyUnauthorizedExternalIO, "different replay payload", "guard-event-1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetImprovementExperiment(ctx, experiment.ID)
	if got.UpdatedAtMS != version {
		t.Fatal("replayed guard event changed the experiment")
	}
}

func TestCandidateActionsRequireCASIdempotencyAndAuditReason(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	experiment := insertExperimentFixture(t, s, source, scope, "actions", model.CandidateStateCanary)
	if _, err := s.ActOnImprovementCandidate(ctx, experiment.CandidateID, "pause", "", "action-1", 2); !errors.Is(err, model.ErrValidation) {
		t.Fatalf("empty audit reason accepted: %v", err)
	}
	if _, err := s.ActOnImprovementCandidate(ctx, experiment.CandidateID, "pause", "operator inspection", "", 2); !errors.Is(err, model.ErrValidation) {
		t.Fatalf("empty idempotency key accepted: %v", err)
	}
	paused, err := s.ActOnImprovementCandidate(ctx, experiment.CandidateID, "pause", "operator inspection", "action-1", 2)
	if err != nil || paused.State != model.CandidateStatePaused || paused.Version != 3 {
		t.Fatalf("valid action failed: %#v, %v", paused, err)
	}
	replayed, err := s.ActOnImprovementCandidate(ctx, experiment.CandidateID, "pause", "replayed request", "action-1", 2)
	if err != nil || replayed.Version != paused.Version || replayed.State != paused.State {
		t.Fatalf("idempotent replay changed result: %#v, %v", replayed, err)
	}
	if _, err = s.ActOnImprovementCandidate(ctx, experiment.CandidateID, "pause", "stale version", "action-2", 2); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("stale expected_version accepted: %v", err)
	}
	var auditEvents int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE aggregate_id=? AND event_type='ImprovementCandidateAction'`, experiment.CandidateID).Scan(&auditEvents); err != nil || auditEvents != 1 {
		t.Fatalf("action audit event count=%d err=%v", auditEvents, err)
	}

	validated := model.ImprovementCandidate{ID: "validated-reject", Type: "prompt", Title: "reject before canary", Rationale: "operator decision", Scope: scope, Patch: json.RawMessage(`{"overlay":"not wanted"}`), SourceTaskID: source.ID, State: model.CandidateStateValidated, Version: 2, CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
	raw, _ := json.Marshal(validated)
	if _, err = s.db.Exec(`INSERT INTO improvement_candidate VALUES(?,?,?,?,?,?,?,?,?)`, validated.ID, validated.Type, validated.State, validated.Version, validated.SourceTaskID, validated.CreatedAtMS, validated.UpdatedAtMS, "validated-reject-fingerprint", raw); err != nil {
		t.Fatal(err)
	}
	rejected, err := s.ActOnImprovementCandidate(ctx, validated.ID, "reject", "not suitable", "validated-reject-action", validated.Version)
	if err != nil || rejected.State != model.CandidateStateRejected || rejected.Version != validated.Version+1 {
		t.Fatalf("validated candidate could not be rejected before canary: %#v, %v", rejected, err)
	}
}

func TestOperatorFieldLockRejectsMatchingFutureCandidatesUntilUnlocked(t *testing.T) {
	ctx := context.Background()
	s, _, source, scope := optimizationWork(t)
	assignment, _, err := s.PrepareTaskOptimization(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	experiment := insertExperimentFixture(t, s, source, scope, "field-lock", model.CandidateStateCanary)
	if _, err = s.ActOnImprovementCandidate(ctx, experiment.CandidateID, "reject_and_lock", "invalid immutable field", "invalid-field-lock-action", 2, []string{"permission"}); !errors.Is(err, model.ErrValidation) {
		t.Fatalf("unsafe extra field lock was accepted: %v", err)
	}
	locked, err := s.ActOnImprovementCandidate(ctx, experiment.CandidateID, "reject_and_lock", "do not let the optimizer alter these fields", "field-lock-action", 2, []string{"stage"})
	if err != nil || locked.State != model.CandidateStateRejected || len(locked.LockedFields) != 2 || !slices.Contains(locked.LockedFields, "overlay") || !slices.Contains(locked.LockedFields, "stage") {
		t.Fatalf("operator field lock was not persisted: %#v, %v", locked, err)
	}

	observation := model.ImprovementObservation{RootTaskID: source.ID, Profile: assignment.Profile, Signals: []model.TaskEfficiencySignal{{Code: "queue_bottleneck", Evidence: "deterministic test"}}}
	proposal := model.CandidateProposal{Type: "prompt", Title: "future prompt", Rationale: "same field in the same scope", Scope: scope, Patch: json.RawMessage(`{"overlay":"another value"}`), Evidence: []model.ExperienceEvidence{{TaskID: source.ID, Signal: "queue_bottleneck"}}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := createCandidateTx(ctx, tx, model.ImprovementJob{RootTaskID: source.ID, Observation: &observation}, proposal)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if blocked.State != model.CandidateStateRejected || !strings.Contains(blocked.ValidationError, "operator-locked") {
		t.Fatalf("matching future candidate bypassed field lock: %#v", blocked)
	}

	unlocked, err := s.ActOnImprovementCandidate(ctx, locked.ID, "unlock_fields", "allow new evidence again", "field-unlock-action", locked.Version)
	if err != nil || len(unlocked.LockedFields) != 0 {
		t.Fatalf("operator field lock could not be removed: %#v, %v", unlocked, err)
	}
	proposal.Title = "future prompt after unlock"
	proposal.Patch = json.RawMessage(`{"overlay":"value after unlock"}`)
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	afterUnlock, err := createCandidateTx(ctx, tx, model.ImprovementJob{RootTaskID: source.ID, Observation: &observation}, proposal)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if afterUnlock.State != model.CandidateStateValidated {
		t.Fatalf("unlocked field remained blocked: %#v", afterUnlock)
	}
}

func TestImprovementJobWaitsForItsOwnRootCompletion(t *testing.T) {
	ctx := context.Background()
	s, _, task, _ := optimizationWork(t)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = enqueueImprovementJobTx(ctx, tx, "ANALYZE", task.ID, "wait-for-own-root", time.Now().UnixMilli(), model.ImprovementObservation{ID: "waiting-observation", RootTaskID: task.ID}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if claimed, claimErr := s.ClaimImprovementJob(ctx, "test-manager", "ANALYZE"); !errors.Is(claimErr, sql.ErrNoRows) || claimed != nil {
		t.Fatalf("active root was eligible for retrospective work: claimed=%#v err=%v", claimed, claimErr)
	}
	if _, err = s.db.Exec(`UPDATE task SET state=? WHERE task_id=?`, model.TaskStateCompleted, task.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimImprovementJob(ctx, "test-manager", "ANALYZE")
	if err != nil || claimed == nil || claimed.RootTaskID != task.ID {
		t.Fatalf("completed root did not release retrospective work: claimed=%#v err=%v", claimed, err)
	}
}
