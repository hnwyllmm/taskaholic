package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func TestTaskRunEventLifecycleAndBackup(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, err := Open(filepath.Join(directory, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	task, replayed, err := state.CreateTask(ctx, "source:42", "Implement feature", "produce and verify a result")
	if err != nil || replayed {
		t.Fatalf("first CreateTask = replayed %v, error %v", replayed, err)
	}
	replayedTask, replayed, err := state.CreateTask(ctx, "source:42", "ignored", "ignored")
	if err != nil || !replayed || replayedTask.ID != task.ID {
		t.Fatalf("idempotent CreateTask = %#v, replayed %v, error %v", replayedTask, replayed, err)
	}

	hello := model.RuntimeHello{
		RuntimeID: "runtime-a", Epoch: "epoch-a", Hostname: "host-a", OS: "test", Arch: "test",
		Capabilities: map[string]any{"adapters": map[string]any{"exec-agent": map[string]any{}}},
	}
	if err := state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	run, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: task.ID, RuntimeID: hello.RuntimeID, AdapterID: "exec-agent", Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	for sequence, event := range []model.RuntimeEvent{
		{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.started", OccurredAt: now},
		{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 2, RunID: run.ID, TaskID: task.ID, Type: "run.progress", OccurredAt: now + 1, Message: "hello", Stream: "stdout"},
		{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 3, RunID: run.ID, TaskID: task.ID, Type: "run.completed", OccurredAt: now + 2, ExitCode: intPointer(0)},
	} {
		duplicate, err := state.ApplyRuntimeEvent(ctx, event)
		if err != nil || duplicate {
			t.Fatalf("ApplyRuntimeEvent %d = duplicate %v, error %v", sequence, duplicate, err)
		}
	}
	duplicate, err := state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 3,
		RunID: run.ID, TaskID: task.ID, Type: "run.completed", ExitCode: intPointer(0),
	})
	if err != nil || !duplicate {
		t.Fatalf("duplicate event = %v, %v", duplicate, err)
	}

	storedRun, err := state.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.State != model.RunStateCompleted || storedRun.ExitCode == nil || *storedRun.ExitCode != 0 {
		t.Fatalf("unexpected completed run: %#v", storedRun)
	}
	storedTask, err := state.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.State != model.TaskStateCompleted {
		t.Fatalf("task state = %s", storedTask.State)
	}
	detail, err := state.GetTaskDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session == nil || detail.Session.ID != run.SessionID {
		t.Fatalf("task session = %#v", detail.Session)
	}
	if len(detail.Runs) != 1 || len(detail.Events) != 8 || detail.Summary == nil {
		t.Fatalf("detail has %d runs and %d events", len(detail.Runs), len(detail.Events))
	}
	if detail.Summary.SourceType != "run" || detail.Summary.SourceID != run.ID || detail.Summary.Metrics.RunCount != 1 {
		t.Fatalf("generic task summary = %#v", detail.Summary)
	}

	backupPath := filepath.Join(directory, "backups", "snapshot.sqlite")
	if err := state.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	backup, err := Open(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	backedUpTask, err := backup.GetTask(ctx, task.ID)
	if err != nil || backedUpTask.State != model.TaskStateCompleted {
		t.Fatalf("backup task = %#v, error %v", backedUpTask, err)
	}
	backedUpDetail, err := backup.GetTaskDetail(ctx, task.ID)
	if err != nil || backedUpDetail.Summary == nil || backedUpDetail.Summary.ID != detail.Summary.ID {
		t.Fatalf("backup summary = %#v, error %v", backedUpDetail.Summary, err)
	}
}

func TestRejectsRuntimeThatDoesNotOwnRun(t *testing.T) {
	ctx := context.Background()
	state, err := Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	task, _, _ := state.CreateTask(ctx, "", "task", "goal")
	for _, runtimeID := range []string{"runtime-a", "runtime-b"} {
		if err := state.RegisterRuntime(ctx, model.RuntimeHello{
			RuntimeID: runtimeID, Epoch: "epoch", Hostname: runtimeID, OS: "test", Arch: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	run, err := state.CreateRun(ctx, CreateRunRequest{
		TaskID: task.ID, RuntimeID: "runtime-a", AdapterID: "exec-agent", Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{
		RuntimeID: "runtime-b", Epoch: "epoch", RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.started",
	})
	if err == nil {
		t.Fatal("event from the wrong runtime was accepted")
	}
}

func intPointer(value int) *int { return &value }
