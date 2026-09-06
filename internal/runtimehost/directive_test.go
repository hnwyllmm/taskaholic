package runtimehost

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
	"work-assistant/internal/rpcpeer"
)

type interactiveTestAdapter struct {
	ready chan struct{}
	once  sync.Once
}

func (a *interactiveTestAdapter) Name() string { return "interactive-test" }

func (a *interactiveTestAdapter) Capabilities() map[string]any {
	return map[string]any{"live_directives": true}
}

func (a *interactiveTestAdapter) Run(ctx context.Context, _ model.RunSpec, _ string, directives <-chan model.Directive, emit func(agent.Event)) agent.Result {
	a.once.Do(func() { close(a.ready) })
	select {
	case directive := <-directives:
		emit(agent.Event{Type: "directive.applied", DirectiveID: directive.ID})
		return agent.Result{ExitCode: 0}
	case <-ctx.Done():
		return agent.Result{ExitCode: -1, Err: ctx.Err()}
	}
}

func TestDaemonDeliversDirectiveToActiveAdapter(t *testing.T) {
	spool, err := OpenSpool(filepath.Join(t.TempDir(), "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	adapter := &interactiveTestAdapter{ready: make(chan struct{})}
	daemon, err := New(Config{
		RuntimeID: "runtime-1", ControlURL: "http://localhost:7337", WorkRoot: t.TempDir(),
	}, spool, nil, adapter)
	if err != nil {
		t.Fatal(err)
	}
	spec := model.RunSpec{
		RunID: "run-1", TaskID: "task-1", SessionID: "session-1", AgentID: "agent-1",
		AdapterID: adapter.Name(), Command: []string{"unused"},
	}
	params, _ := json.Marshal(spec)
	if _, rpcErr := daemon.handleRunStart(context.Background(), rpcpeer.Request{
		ID: "message-1", Method: "run.start", Params: params,
	}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	select {
	case <-adapter.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("adapter did not start")
	}
	directive := model.Directive{
		ID: "directive-1", RunID: spec.RunID, TaskID: spec.TaskID, SessionID: spec.SessionID,
		Kind: model.DirectiveKindMessage, Message: "adjust the plan",
	}
	directiveParams, _ := json.Marshal(directive)
	if _, rpcErr := daemon.handleRunDirective(context.Background(), rpcpeer.Request{
		ID: directive.ID, Method: "run.directive", Params: directiveParams,
	}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	done := make(chan struct{})
	go func() {
		daemon.activeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("adapter did not finish")
	}

	foundApplied := false
	for {
		message, err := spool.NextOutbound(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if message == nil {
			break
		}
		var event model.RuntimeEvent
		if err := json.Unmarshal(message.Params, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "directive.applied" && event.DirectiveID == directive.ID {
			foundApplied = true
		}
		if err := spool.MarkOutboundDelivered(context.Background(), message.RuntimeSeq); err != nil {
			t.Fatal(err)
		}
	}
	if !foundApplied {
		t.Fatal("runtime did not durably enqueue directive.applied")
	}
}
