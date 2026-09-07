package taskaction

import (
	"context"
	"errors"
	"testing"

	"work-assistant/internal/gitlabci"
	"work-assistant/internal/model"
)

type actionState struct {
	p           model.TestPipeline
	maintenance string
	claim       bool
}

func (s *actionState) Maintenance(context.Context) (string, error) { return s.maintenance, nil }
func (s *actionState) PendingTestActions(context.Context) ([]model.TestPipeline, error) {
	switch s.p.State {
	case "QUEUED", "SUBMITTING", "UNCERTAIN":
		return []model.TestPipeline{s.p}, nil
	}
	return nil, nil
}
func (s *actionState) ClaimTestAction(context.Context, string) (bool, error) {
	if !s.claim || s.p.State != "QUEUED" {
		return false, nil
	}
	s.p.State = "SUBMITTING"
	return true, nil
}
func (s *actionState) DeferTestAction(_ context.Context, _ string, message string) error {
	if s.p.State == "SUBMITTING" {
		s.p.State = "UNCERTAIN"
	}
	s.p.Error = message
	return nil
}
func (s *actionState) FailTestAction(_ context.Context, _ string, state, message string) error {
	s.p.State = state
	s.p.Error = message
	return nil
}
func (s *actionState) AttachTestPipeline(_ context.Context, _ string, o model.PipelineObservation) error {
	s.p.PipelineID = o.ID
	s.p.State = "created"
	return nil
}

type actionClient struct {
	creates, finds int
	err            error
	found          *model.PipelineObservation
	state          *actionState
}

func (c *actionClient) Create(context.Context, model.TestPipeline) (model.PipelineObservation, error) {
	if c.state.p.State != "SUBMITTING" {
		panic("POST before durable claim")
	}
	c.creates++
	return model.PipelineObservation{ID: 42}, c.err
}
func (c *actionClient) Find(context.Context, model.TestPipeline) (*model.PipelineObservation, error) {
	c.finds++
	return c.found, nil
}
func actionFixture() (*PipelineActions, *actionState, *actionClient) {
	s := &actionState{p: model.TestPipeline{ID: "one", Kind: model.SeekDBTestKind, State: "QUEUED", HeadSHA: "sha"}, claim: true}
	c := &actionClient{state: s}
	a := NewPipelines(s, c)
	a.LatestPR = func(context.Context, string) (string, string, error) { return "sha", "open", nil }
	return a, s, c
}
func TestActionUncertainSubmissionOnlyReconcilesAfterRestart(t *testing.T) {
	a, s, c := actionFixture()
	c.err = errors.New("response lost after server accepted POST")
	ctx := context.Background()
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if s.p.State != "UNCERTAIN" || c.creates != 1 {
		t.Fatal(s.p, c.creates)
	}
	// New executor process over the persisted uncertain record.
	a = NewPipelines(s, c)
	a.LatestPR = func(context.Context, string) (string, string, error) {
		panic("reconcile must not require PR to remain open")
	}
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if c.creates != 1 || c.finds != 1 {
		t.Fatal("retried POST")
	}
	c.found = &model.PipelineObservation{ID: 42}
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if c.creates != 1 || s.p.PipelineID != 42 {
		t.Fatal("recovery did not attach original pipeline")
	}
}
func TestActionDoesNotLaunchStaleClosedPausedOrUnverifiedPR(t *testing.T) {
	for _, kind := range []string{"stale", "closed", "network", "pause", "maintenance", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			a, s, c := actionFixture()
			switch kind {
			case "stale":
				a.LatestPR = func(context.Context, string) (string, string, error) { return "new-sha", "open", nil }
			case "closed":
				a.LatestPR = func(context.Context, string) (string, string, error) { return "sha", "closed", nil }
			case "network":
				a.LatestPR = func(context.Context, string) (string, string, error) { return "", "", errors.New("cannot verify") }
			case "pause":
				s.claim = false
			case "maintenance":
				s.maintenance = "upgrade"
			case "rejected":
				c.err = &gitlabci.HTTPError{Status: 403}
			}
			if err := a.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "rejected" {
				want = 1
			}
			if c.creates != want {
				t.Fatal("unsafe external mutation", kind, c.creates)
			}
			if kind == "rejected" && s.p.State != "ERROR" {
				t.Fatal(s.p)
			}
		})
	}
}
