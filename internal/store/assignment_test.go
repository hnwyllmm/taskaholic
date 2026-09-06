package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func TestDeferredWorkIsDurableAndNeverRunsBeforeAssignment(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Saved task", Goal: "Decide executor later", DeferAssignment: true, Key: "saved"})
	if err != nil || task.State != model.TaskStateNew {
		t.Fatal(task, err)
	}
	if _, err = s.MessageWork(ctx, task.ID, "More requirements", "more", false); err != nil {
		t.Fatal(err)
	}
	if err = s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MessageWork(ctx, task.ID, "Still not assigned", "more-2", false); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.sqlite")
	if err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, file := range []string{path, backup} {
		reopened, err := Open(file)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := reopened.PendingWork(ctx)
		if err != nil || len(pending) != 0 {
			t.Fatal("saved task scheduled after reopen", pending, err)
		}
		current, err := reopened.GetTask(ctx, task.ID)
		if err != nil || current.State != model.TaskStateNew {
			t.Fatal(current, err)
		}
		work, err := reopened.GetWorkDetail(ctx, task.ID)
		if err != nil || !work.Config.Paused || work.Config.TaskVersion != current.Version || len(work.Messages) < 3 {
			t.Fatal(work, err)
		}
		if _, err = reopened.StartWorkRun(ctx, CreateRunRequest{TaskID: task.ID}, workflow.JSONContract{}); !errors.Is(err, model.ErrConflict) {
			t.Fatal("bypassed assignment", err)
		}
		current, err = reopened.AssignWork(ctx, task.ID, "", current.Version)
		if err != nil || current.State != model.TaskStateQueued {
			t.Fatal(current, err)
		}
		pending, err = reopened.PendingWork(ctx)
		if err != nil || len(pending) != 1 || pending[0] != task.ID {
			t.Fatal("assignment not queued", pending, err)
		}
		reopened.Close()
	}
}

func TestWorkAssignmentValidatesAndCanSwitchUntilFirstSession(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	other := secondMember(t, s, a)
	if err := s.MarkAllRuntimesOffline(ctx); err != nil {
		t.Fatal(err)
	}
	assigned, err := s.AssignWork(ctx, task.ID, a.ID, task.Version)
	if err != nil {
		t.Fatal("offline member must remain selectable", err)
	}
	config, _ := s.GetWorkConfig(ctx, task.ID)
	if config.AgentID != a.ID || config.TaskVersion != assigned.Version {
		t.Fatal(config)
	}
	listed, err := s.ListWork(ctx)
	if err != nil || len(listed) != 1 || listed[0].PreferredAgentID != a.ID || listed[0].AssignedAgentID != "" {
		t.Fatal("queued member not visible, or incorrectly pinned", listed, err)
	}
	if _, err = s.AssignWork(ctx, task.ID, other.ID, task.Version); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale assignment accepted", err)
	}
	if _, err = s.AssignWork(ctx, task.ID, "", 0); !errors.Is(err, model.ErrValidation) {
		t.Fatal("missing version accepted", err)
	}
	automatic, err := s.AssignWork(ctx, task.ID, "", assigned.Version)
	if err != nil {
		t.Fatal(err)
	}
	config, _ = s.GetWorkConfig(ctx, task.ID)
	if config.AgentID != "" || config.Paused || config.SchedulerError != "" {
		t.Fatal(config)
	}
	if _, err = s.StartWorkRun(ctx, CreateRunRequest{TaskID: task.ID, AgentID: a.ID, ExpectedTaskVersion: assigned.Version}, workflow.JSONContract{}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("old scheduler snapshot accepted", err)
	}
	if automatic.Version != assigned.Version+1 {
		t.Fatal("version did not advance", automatic)
	}

	// Invalid role/capability choices fail atomically, even if explicitly named.
	wrongRole := publishTestRole(t, s, "security.review")
	bad, err := s.CreateAgent(ctx, model.AgentProfile{Name: "wrong-role", RoleID: wrongRole.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AssignWork(ctx, task.ID, bad.ID, automatic.Version); !errors.Is(err, model.ErrValidation) {
		t.Fatal("wrong role accepted", err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if current.Version != automatic.Version {
		t.Fatal("failed assignment changed task", current)
	}
	if _, err = s.CreateWork(ctx, CreateWorkRequest{Title: "Wrong choice", Goal: "Work", AgentID: bad.ID, Requirements: task.Requirements}); !errors.Is(err, model.ErrValidation) {
		t.Fatal("creation bypassed constraints", err)
	}
}

func TestAssignmentAndSchedulerRaceDoesNotChangeSession(t *testing.T) {
	ctx := context.Background()
	for range 8 {
		s, a, task := workFixture(t)
		b := secondMember(t, s, a)
		var assignErr, runErr error
		var run model.Run
		var wg sync.WaitGroup
		wg.Go(func() { _, assignErr = s.AssignWork(ctx, task.ID, b.ID, task.Version) })
		wg.Go(func() {
			run, runErr = s.StartWorkRun(ctx, CreateRunRequest{TaskID: task.ID, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID, ExpectedTaskVersion: task.Version}, workflow.JSONContract{})
		})
		wg.Wait()
		if (assignErr == nil) == (runErr == nil) {
			t.Fatal("race must have exactly one winner", assignErr, runErr)
		}
		if runErr == nil {
			if err := s.PauseWork(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			current, _ := s.GetTask(ctx, task.ID)
			for _, target := range []string{"", a.ID, b.ID} {
				if _, err := s.AssignWork(ctx, task.ID, target, current.Version); !errors.Is(err, model.ErrConflict) {
					t.Fatal("bound task reassigned", err)
				}
			}
			session, err := s.GetTaskSession(ctx, task.ID)
			if err != nil || session.ID != run.SessionID || session.AgentID != a.ID {
				t.Fatal("session affinity lost", session, err)
			}
		} else {
			config, _ := s.GetWorkConfig(ctx, task.ID)
			if config.AgentID != b.ID {
				t.Fatal(config)
			}
			detail, _ := s.GetTaskDetail(ctx, task.ID)
			if len(detail.Runs) != 0 || detail.Session != nil {
				t.Fatal("losing scheduler left partial run", detail)
			}
		}
	}
}

func TestExplicitAssignmentSupersedesInFlightAIRouter(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	executor := secondMember(t, s, a)
	binding := bindSystem(t, s, "task_router", executor)
	d, err := s.StartRoutingDecision(ctx, task.ID, task.Version, binding, executor, []model.AgentProfile{a})
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := s.AssignWork(ctx, task.ID, a.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := s.GetRoutingDecision(ctx, task.ID, task.Version)
	if err != nil || decision.State != "SUPERSEDED" {
		t.Fatal(decision, err)
	}
	var interruptions int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM directive WHERE run_id=?`, d.RunID).Scan(&interruptions); err != nil || interruptions != 1 {
		t.Fatal("missing durable router cancellation", interruptions, err)
	}
	output, _ := json.Marshal(map[string]string{"agent_id": a.ID, "reason": "late answer"})
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: executor.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: d.RunID, Type: "run.completed", Output: string(output)}); err != nil {
		t.Fatal(err)
	}
	decision, _ = s.GetRoutingDecision(ctx, task.ID, task.Version)
	if decision.State != "SUPERSEDED" {
		t.Fatal("late router result replaced user choice", decision)
	}
	run, err := s.StartWorkRun(ctx, CreateRunRequest{TaskID: task.ID, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID, ExpectedTaskVersion: assigned.Version}, workflow.JSONContract{})
	if err != nil || run.AgentID != a.ID {
		t.Fatal(run, err)
	}
}
