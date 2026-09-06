package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func TestTaskKeepsAgentSessionAffinityAndOpaqueMemoryReference(t *testing.T) {
	ctx := context.Background()
	state, err := Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	registerTestRuntime(t, state, "runtime-a", "epoch-a")
	registerTestRuntime(t, state, "runtime-b", "epoch-b")
	task, _, err := state.CreateTask(ctx, "", "session affinity", "resume with the same agent")
	if err != nil {
		t.Fatal(err)
	}
	first, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: task.ID, RuntimeID: "runtime-a", AdapterID: "exec-agent", ModelID: "model-a", Command: []string{"true"},
		IdempotencyKey: "run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID == "" || first.AgentID == "" || first.AdapterID != "exec-agent" || first.ModelID != "model-a" {
		t.Fatalf("first run ownership = %#v", first)
	}
	replayed, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: task.ID, RuntimeID: "runtime-a", AdapterID: "exec-agent",
		Command: []string{"this", "must", "not", "run"}, IdempotencyKey: "run-1",
	})
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("idempotent run = %#v, error %v", replayed, err)
	}
	if _, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: task.ID, RuntimeID: "runtime-b", AdapterID: "exec-agent", Command: []string{"true"},
	}); err == nil {
		t.Fatal("session affinity allowed an implicit runtime move")
	}
	if _, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: task.ID, AdapterID: "exec-agent", Command: []string{"true"},
	}); err == nil {
		t.Fatal("session allowed concurrent runs")
	}
	now := time.Now().UTC().UnixMilli()
	for _, event := range []model.RuntimeEvent{
		{RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: 10, RunID: first.ID, TaskID: task.ID, SessionID: first.SessionID, Type: "run.started", OccurredAt: now},
		{RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: 11, RunID: first.ID, TaskID: task.ID, SessionID: first.SessionID, Type: "run.completed", OccurredAt: now + 1, ExitCode: intPointer(0)},
	} {
		if _, err := state.ApplyRuntimeEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	second, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: task.ID, AdapterID: "exec-agent", Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID || second.AgentID != first.AgentID ||
		second.RuntimeID != first.RuntimeID || second.ModelID != first.ModelID {
		t.Fatalf("second run lost affinity: first %#v, second %#v", first, second)
	}

	reference := "codex:thread-123"
	duplicate, err := state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: 12, RunID: second.ID,
		TaskID: task.ID, SessionID: second.SessionID, Type: "session.bound", AgentSessionRef: reference,
	})
	if err != nil || duplicate {
		t.Fatalf("session.bound = duplicate %v, error %v", duplicate, err)
	}
	session, err := state.GetTaskSession(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.AgentSessionRef != reference || session.MemoryOwner != "agent" || session.ModelID != "model-a" {
		t.Fatalf("session = %#v", session)
	}
}

func TestDirectiveLifecycle(t *testing.T) {
	ctx := context.Background()
	state, err := Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	registerTestRuntime(t, state, "runtime-a", "epoch-a")
	task, _, _ := state.CreateTask(ctx, "", "directive", "guide a running agent")
	run, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: task.ID, RuntimeID: "runtime-a", AdapterID: "exec-agent", Command: []string{"sleep", "10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	directive, err := state.CreateDirective(ctx, run.ID, model.DirectiveKindMessage, "change direction", "guide-1")
	if err != nil {
		t.Fatal(err)
	}
	if directive.State != model.DirectiveStateQueued || directive.SessionID != run.SessionID {
		t.Fatalf("directive = %#v", directive)
	}
	replayed, err := state.CreateDirective(ctx, run.ID, model.DirectiveKindMessage, "duplicate", "guide-1")
	if err != nil || replayed.ID != directive.ID {
		t.Fatalf("idempotent directive = %#v, error %v", replayed, err)
	}
	_, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: 1, RunID: run.ID,
		TaskID: task.ID, SessionID: run.SessionID, DirectiveID: directive.ID,
		Type: "directive.rejected", Error: "adapter does not support live input",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.GetDirective(ctx, directive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != model.DirectiveStateRejected || stored.AppliedAtMS == nil || stored.Error == "" {
		t.Fatalf("stored directive = %#v", stored)
	}
}

func TestDynamicSubtasksWakeParentAfterAllChildrenFinish(t *testing.T) {
	ctx := context.Background()
	state, err := Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	registerTestRuntime(t, state, "runtime-a", "epoch-a")
	parent, _, _ := state.CreateTask(ctx, "", "parent", "split work dynamically")
	parentRun, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: parent.ID, RuntimeID: "runtime-a", AdapterID: "exec-agent", Command: []string{"orchestrate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, replayed, err := state.CreateSubtask(ctx, parent.ID, parentRun.ID, "split-1", "child one", "first part")
	if err != nil || replayed {
		t.Fatalf("first subtask = %#v, replayed %v, error %v", first, replayed, err)
	}
	replayedFirst, replayed, err := state.CreateSubtask(ctx, parent.ID, parentRun.ID, "split-1", "ignored", "ignored")
	if err != nil || !replayed || replayedFirst.Task.ID != first.Task.ID {
		t.Fatalf("replayed subtask = %#v, replayed %v, error %v", replayedFirst, replayed, err)
	}
	second, _, err := state.CreateSubtask(ctx, parent.ID, parentRun.ID, "split-2", "child two", "second part")
	if err != nil {
		t.Fatal(err)
	}
	storedParent, _ := state.GetTask(ctx, parent.ID)
	if storedParent.State != model.TaskStateWaiting {
		t.Fatalf("parent state after split = %s", storedParent.State)
	}
	now := time.Now().UTC().UnixMilli()
	for _, event := range []model.RuntimeEvent{
		{RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: 1, RunID: parentRun.ID, TaskID: parent.ID, SessionID: parentRun.SessionID, Type: "run.started", OccurredAt: now},
		{RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: 2, RunID: parentRun.ID, TaskID: parent.ID, SessionID: parentRun.SessionID, Type: "run.completed", OccurredAt: now + 1, ExitCode: intPointer(0)},
	} {
		if _, err := state.ApplyRuntimeEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	storedParent, _ = state.GetTask(ctx, parent.ID)
	if storedParent.State != model.TaskStateWaiting {
		t.Fatalf("orchestrator run incorrectly completed parent with open children: %s", storedParent.State)
	}

	completeTask(t, state, first.Task.ID, 10)
	storedParent, _ = state.GetTask(ctx, parent.ID)
	if storedParent.State != model.TaskStateWaiting {
		t.Fatalf("parent woke before every child completed: %s", storedParent.State)
	}
	completeTask(t, state, second.Task.ID, 20)
	storedParent, _ = state.GetTask(ctx, parent.ID)
	if storedParent.State != model.TaskStateAssigned {
		t.Fatalf("parent state after all children = %s", storedParent.State)
	}
	detail, err := state.GetTaskDetail(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Edges) != 2 {
		t.Fatalf("parent edges = %#v", detail.Edges)
	}
}

func TestDynamicSubtasksNotifyAWaitingParentAgent(t *testing.T) {
	ctx := context.Background()
	state, err := Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	registerTestRuntime(t, state, "runtime-a", "epoch-a")
	parent, _, _ := state.CreateTask(ctx, "", "live parent", "wait for subagents")
	parentRun, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: parent.ID, RuntimeID: "runtime-a", AdapterID: "exec-agent", Command: []string{"wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: 1, RunID: parentRun.ID,
		TaskID: parent.ID, SessionID: parentRun.SessionID, Type: "run.started",
	}); err != nil {
		t.Fatal(err)
	}
	first, _, err := state.CreateSubtask(ctx, parent.ID, parentRun.ID, "live-child-1", "first", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := state.CreateSubtask(ctx, parent.ID, parentRun.ID, "live-child-2", "second", "second")
	if err != nil {
		t.Fatal(err)
	}
	completeTask(t, state, first.Task.ID, 10)
	completeTask(t, state, second.Task.ID, 20)
	storedParent, err := state.GetTask(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedParent.State != model.TaskStateInProgress {
		t.Fatalf("live parent state = %s", storedParent.State)
	}
	directives, err := state.ListDirectivesForTask(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(directives) != 1 || directives[0].RunID != parentRun.ID ||
		!strings.Contains(directives[0].Message, "subtasks.settled") ||
		!strings.Contains(directives[0].Message, first.Task.ID) ||
		!strings.Contains(directives[0].Message, second.Task.ID) {
		t.Fatalf("parent directives = %#v", directives)
	}
}

func registerTestRuntime(t *testing.T, state *Store, runtimeID, epoch string) {
	t.Helper()
	err := state.RegisterRuntime(context.Background(), model.RuntimeHello{
		RuntimeID: runtimeID, Epoch: epoch, Hostname: runtimeID, OS: "test", Arch: "test",
		Capabilities: map[string]any{"adapters": map[string]any{"exec-agent": map[string]any{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func completeTask(t *testing.T, state *Store, taskID string, sequence int64) {
	t.Helper()
	run, err := state.CreateRun(context.Background(), CreateRunRequest{
		TaskID: taskID, RuntimeID: "runtime-a", AdapterID: "exec-agent", Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	for _, event := range []model.RuntimeEvent{
		{RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: sequence, RunID: run.ID, TaskID: taskID, SessionID: run.SessionID, Type: "run.started", OccurredAt: now},
		{RuntimeID: "runtime-a", Epoch: "epoch-a", RuntimeSeq: sequence + 1, RunID: run.ID, TaskID: taskID, SessionID: run.SessionID, Type: "run.completed", OccurredAt: now + 1, ExitCode: intPointer(0)},
	} {
		if _, err := state.ApplyRuntimeEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
}
