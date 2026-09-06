package runtimehost

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
)

func TestSpoolDeduplicatesCommandsAndSequencesEvents(t *testing.T) {
	ctx := context.Background()
	spool, err := OpenSpool(filepath.Join(t.TempDir(), "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	spec := model.RunSpec{RunID: "run-1", TaskID: "task-1", AgentID: "exec-agent", Command: []string{"true"}}
	params, _ := json.Marshal(spec)
	duplicate, err := spool.AcceptRunStart(ctx, "message-1", params, spec, "/tmp/work")
	if err != nil || duplicate {
		t.Fatalf("first command = duplicate %v, error %v", duplicate, err)
	}
	duplicate, err = spool.AcceptRunStart(ctx, "message-1", params, spec, "/tmp/work")
	if err != nil || !duplicate {
		t.Fatalf("second command = duplicate %v, error %v", duplicate, err)
	}
	for index, eventType := range []string{"run.started", "run.completed"} {
		sequence, err := spool.EnqueueEvent(ctx, model.RuntimeEvent{
			RuntimeID: "runtime-1", Epoch: "epoch-1", RunID: spec.RunID, TaskID: spec.TaskID, Type: eventType,
		})
		if err != nil || sequence != int64(index+1) {
			t.Fatalf("event %d sequence = %d, error %v", index, sequence, err)
		}
	}
	first, err := spool.NextOutbound(ctx)
	if err != nil || first == nil || first.RuntimeSeq != 1 {
		t.Fatalf("first outbound = %#v, error %v", first, err)
	}
	if err := spool.MarkOutboundDelivered(ctx, first.RuntimeSeq); err != nil {
		t.Fatal(err)
	}
	second, err := spool.NextOutbound(ctx)
	if err != nil || second == nil || second.RuntimeSeq != 2 {
		t.Fatalf("second outbound = %#v, error %v", second, err)
	}
	var decoded model.RuntimeEvent
	if err := json.Unmarshal(second.Params, &decoded); err != nil || decoded.RuntimeSeq != 2 {
		t.Fatalf("encoded event = %#v, error %v", decoded, err)
	}

	recovered, err := spool.RecoverActiveRuns(ctx, "runtime-1", "epoch-2")
	if err != nil || len(recovered) != 1 || recovered[0].RunID != spec.RunID {
		t.Fatalf("recovered = %#v, error %v", recovered, err)
	}
	recovered, err = spool.RecoverActiveRuns(ctx, "runtime-1", "epoch-2")
	if err != nil || len(recovered) != 0 {
		t.Fatalf("second recovery = %#v, error %v", recovered, err)
	}
}

func TestWorkingDirectoryCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	spool, err := OpenSpool(filepath.Join(t.TempDir(), "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	daemon, err := New(Config{RuntimeID: "runtime-1", ControlURL: "http://localhost:7337", WorkRoot: root}, spool, nil, agent.ExecAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.resolveWorkingDir(model.RunSpec{RunID: "run-1", WorkingDir: "../escape"}); err == nil {
		t.Fatal("relative path escape was accepted")
	}
	resolved, err := daemon.resolveWorkingDir(model.RunSpec{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Join(realRoot, "runs", "run-1") {
		t.Fatalf("resolved path = %s", resolved)
	}
}

func TestRuntimeWebSocketURL(t *testing.T) {
	got, err := runtimeWebSocketURL("https://control.example/base/")
	if err != nil || got != "wss://control.example/base/runtime/ws" {
		t.Fatalf("url = %q, error %v", got, err)
	}
}
