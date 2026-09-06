package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"work-assistant/internal/model"
)

func TestOutboxFromPreviousEpochCanReplayOnlyOverCurrentConnection(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task, _, err := s.CreateTask(ctx, "key", "Offline task", "retain output across runtime restart")
	if err != nil {
		t.Fatal(err)
	}
	hello := model.RuntimeHello{RuntimeID: "runtime-1", Epoch: "epoch-old", Capabilities: map[string]any{"adapters": map[string]any{"exec-agent": map[string]any{}}}}
	if err := s.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: hello.RuntimeID, AdapterID: "exec-agent", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	event := model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.completed", Output: "离线时已保存的结果"}
	hello.Epoch = "epoch-new"
	if err := s.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRuntimeEventFrom(ctx, "epoch-old", event); err == nil {
		t.Fatal("stale connection accepted")
	}
	if _, err := s.ApplyRuntimeEventFrom(ctx, "epoch-new", event); err != nil {
		t.Fatal("durable old outbox cannot replay", err)
	}
	if duplicate, err := s.ApplyRuntimeEventFrom(ctx, "epoch-new", event); err != nil || !duplicate {
		t.Fatal("replay not idempotent", err)
	}
	saved, err := s.GetRun(ctx, run.ID)
	if err != nil || saved.Output != event.Output || saved.State != model.RunStateCompleted {
		t.Fatalf("lost output: %+v %v", saved, err)
	}
}

func TestProductionOpenRefusesMissingData(t *testing.T) {
	t.Setenv("ASSISTANT_BACKUP_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := OpenProtected(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenProtected(path); err == nil {
		reopened.Close()
		t.Fatal("production open created an empty replacement")
	}
}
