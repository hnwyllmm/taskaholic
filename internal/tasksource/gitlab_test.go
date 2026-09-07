package tasksource

import (
	"context"
	"encoding/json"
	"testing"

	"work-assistant/internal/model"
)

type observedPipeline struct{ status string }

func (o *observedPipeline) Observe(context.Context, model.TestPipeline) (model.PipelineObservation, error) {
	return model.PipelineObservation{ID: 42, Status: o.status}, nil
}
func TestPipelineSourceOnlyEmitsChangedObservationsAndStopsOnTerminal(t *testing.T) {
	reader := &observedPipeline{status: "running"}
	p := model.TestPipeline{ID: "request", TaskID: "owner", PollTargetID: "watch", PipelineID: 42, URL: model.PipelineURL(42), HeadSHA: "sha"}
	g := &GitLab{Read: reader, Lookup: func(context.Context, string) (model.TestPipeline, error) { return p, nil }}
	target := model.SourceTarget{ID: "watch", TaskID: "owner", Entity: p.URL, TestRequestID: p.ID, Cursor: json.RawMessage(`{}`)}
	result, err := g.Poll(context.Background(), model.TaskSource{}, target)
	if err != nil || len(result.Events) != 1 || result.Closed {
		t.Fatal(result, err)
	}
	target.Cursor = result.Cursor
	result, err = g.Poll(context.Background(), model.TaskSource{}, target)
	if err != nil || len(result.Events) != 0 {
		t.Fatal("unchanged poll produced work", result, err)
	}
	reader.status = "failed"
	result, err = g.Poll(context.Background(), model.TaskSource{}, target)
	if err != nil || len(result.Events) != 1 || !result.Closed || result.Events[0].Kind != "gitlab.pipeline" {
		t.Fatal(result, err)
	}
	target.TaskID = "unrelated"
	if _, err = g.Poll(context.Background(), model.TaskSource{}, target); err == nil {
		t.Fatal("watch stole another task")
	}
}
