package runtimehost

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
	"work-assistant/internal/rpcpeer"
)

func acceptedFixture(t *testing.T) (*Spool, model.RunSpec) {
	t.Helper()
	s, err := OpenSpool(filepath.Join(t.TempDir(), "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	spec := model.RunSpec{RunID: "run-1", TaskID: "task-1", AgentID: "exec-agent", Command: []string{"true"}}
	raw, _ := json.Marshal(spec)
	if _, err := s.AcceptRunStart(context.Background(), "message-1", raw, spec, "/tmp/work"); err != nil {
		t.Fatal(err)
	}
	return s, spec
}

func TestTerminalOutputAndStatusCommitTogetherAndReplayIsIdempotent(t *testing.T) {
	s, spec := acceptedFixture(t)
	ctx := context.Background()
	event := model.RuntimeEvent{RuntimeID: "runtime-1", Epoch: "epoch-1", RunID: spec.RunID, TaskID: spec.TaskID, Type: "run.completed", Output: "不能丢失的交付物"}
	if _, err := s.db.Exec("CREATE TRIGGER inject_failure BEFORE UPDATE ON local_run BEGIN SELECT RAISE(ABORT, 'injected disk error'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRun(ctx, "message-1", "COMPLETED", event); err == nil {
		t.Fatal("expected injected failure")
	}
	var state, status string
	var count int
	s.db.QueryRow("SELECT state FROM local_run").Scan(&state)
	s.db.QueryRow("SELECT status FROM inbound_message").Scan(&status)
	s.db.QueryRow("SELECT COUNT(*) FROM outbound_message").Scan(&count)
	if state != "ACCEPTED" || status != "ACCEPTED" || count != 0 {
		t.Fatalf("partial transaction: %s %s %d", state, status, count)
	}
	if _, err := s.db.Exec("DROP TRIGGER inject_failure"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.CompleteRun(ctx, "message-1", "COMPLETED", event); err != nil {
			t.Fatal(err)
		}
	}
	s.db.QueryRow("SELECT COUNT(*) FROM outbound_message").Scan(&count)
	if count != 1 {
		t.Fatal("duplicate terminal event", count)
	}
	msg, err := s.NextOutbound(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var saved model.RuntimeEvent
	if err := json.Unmarshal(msg.Params, &saved); err != nil || saved.Output != event.Output {
		t.Fatal("lost output", err)
	}
	runs, err := s.RecoverActiveRuns(ctx, "runtime-1", "epoch-2")
	if err != nil || len(runs) != 0 {
		t.Fatal("completed work was rerun", err)
	}
}

func TestRecoveryFailureCannotLoseRecoveryEvent(t *testing.T) {
	s, _ := acceptedFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec("CREATE TRIGGER inject_failure BEFORE INSERT ON outbound_message BEGIN SELECT RAISE(ABORT, 'injected full disk'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverActiveRuns(ctx, "runtime-1", "epoch-2"); err == nil {
		t.Fatal("expected recovery persistence failure")
	}
	var state string
	if err := s.db.QueryRow("SELECT state FROM local_run").Scan(&state); err != nil || state != "ACCEPTED" {
		t.Fatal("unrecoverable run", state, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER inject_failure"); err != nil {
		t.Fatal(err)
	}
	runs, err := s.RecoverActiveRuns(ctx, "runtime-1", "epoch-2")
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	msg, err := s.NextOutbound(ctx)
	if err != nil || msg == nil {
		t.Fatal("lost recovery notification", err)
	}
	runs, err = s.RecoverActiveRuns(ctx, "runtime-1", "epoch-3")
	if err != nil || len(runs) != 0 {
		t.Fatal("repeated recovery", err)
	}
}

func TestRuntimeStopsAcceptingWorkAfterStorageFailure(t *testing.T) {
	s, _ := acceptedFixture(t)
	d, err := New(Config{RuntimeID: "runtime-1", ControlURL: "http://localhost:7337", WorkRoot: t.TempDir()}, s, nil, agent.ExecAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER inject_failure BEFORE INSERT ON outbound_message BEGIN SELECT RAISE(ABORT, 'injected full disk'); END"); err != nil {
		t.Fatal(err)
	}
	if err := d.emit(context.Background(), model.RuntimeEvent{RunID: "run-1", Type: "run.progress"}); err == nil {
		t.Fatal("event failure was hidden")
	}
	if _, err := d.handleRunStart(context.Background(), rpcpeer.Request{ID: "next", Method: "run.start", Params: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("unhealthy runtime accepted new work")
	}
}
