package tasksource

import (
	"context"
	"encoding/json"
	"testing"

	"work-assistant/internal/model"
)

type observedPipeline struct{ status string }

func (o *observedPipeline) Observe(context.Context, model.TestPipeline) (model.PipelineObservation, error) {
	result := model.PipelineObservation{ID: 42, Status: o.status}
	if o.status == "failed" {
		result.Jobs = []model.PipelineJob{{ID: 7, Status: "failed", LogCollected: true, LogExcerpt: "assertion failed"}}
	}
	return result, nil
}

func TestPipelineSourcePublishesLateFailureEvidenceOnce(t *testing.T) {
	reader := &observedPipeline{status: "failed"}
	p := model.TestPipeline{ID: "request", TaskID: "owner", PollTargetID: "watch", PipelineID: 42, URL: model.PipelineURL(42), HeadSHA: "sha", State: "failed", Jobs: []model.PipelineJob{{ID: 7, Status: "failed"}}}
	g := &GitLab{Read: reader, Lookup: func(context.Context, string) (model.TestPipeline, error) { return p, nil }}
	target := model.SourceTarget{ID: "watch", TaskID: "owner", Entity: p.URL, TestRequestID: p.ID, Cursor: json.RawMessage(`{"status":"failed","seq":1}`)}
	result, err := g.Poll(context.Background(), model.TaskSource{}, target)
	if err != nil || len(result.Events) != 1 || !result.Closed || !model.PipelineFailureEvidenceReady(result.Events[0].Pipeline.Jobs) {
		t.Fatal(result, err)
	}
	p.Jobs = result.Events[0].Pipeline.Jobs
	target.Cursor = result.Cursor
	result, err = g.Poll(context.Background(), model.TaskSource{}, target)
	if err != nil || len(result.Events) != 0 {
		t.Fatal("same evidence was emitted twice", result, err)
	}
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
