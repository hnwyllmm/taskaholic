package tasksource

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

type observedIssue struct{}

func (observedIssue) Poll(context.Context, model.TaskSource, model.SourceTarget) (PollResult, error) {
	return PollResult{Cursor: json.RawMessage(`{"revision":1}`), Events: []model.SourceEvent{{
		Key: "issue:1:v1", Kind: "antmultica.issue", Entity: "workspace:1", Title: "write a document", Message: "Prepare documentation",
	}}}, nil
}

func TestPollingOnlyPersistsEventsManagerCreatesUnassignedWork(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source, err := s.SaveTaskSource(ctx, model.TaskSource{ID: "ant", Kind: "antmultica", Name: "My issues", Enabled: true, IntervalSeconds: 5,
		Config: model.SourceConfig{WorkspaceID: "workspace", WorkspaceSlug: "seekdb", AssigneeID: "me", IterationKey: "迭代", IterationValue: "1.5.0"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Store: s, Providers: map[string]Provider{"antmultica": observedIssue{}}}
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.ListWork(ctx)
	if err != nil || len(tasks) != 0 {
		t.Fatal("poller performed Manager work", tasks, err)
	}
	events, err := s.ListSourceEvents(ctx)
	if err != nil || len(events) != 1 || events[0].State != "PENDING" {
		t.Fatal(events, err)
	}
	// Turning off collection cannot cancel previously accepted events.
	source.Enabled = false
	if _, err = s.SaveTaskSource(ctx, source, source.Version); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	tasks, err = s.ListWork(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatal(tasks, err)
	}
	task := tasks[0]
	if task.AssignedAgentID != "" || !task.Requirements.IsEmpty() || task.State != model.TaskStateQueued {
		t.Fatal("source selected execution policy", task)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	tasks, err = s.ListWork(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatal("event replay created duplicate work", tasks, err)
	}
}
