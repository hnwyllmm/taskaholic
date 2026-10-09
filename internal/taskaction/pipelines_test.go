package taskaction

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/gitlabci"
	"work-assistant/internal/model"
)

type actionState struct {
	p                model.TestPipeline
	maintenance      string
	claim            bool
	evidenceAttached bool
	retryCompleted   bool
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
func (s *actionState) PendingTestRetries(context.Context) ([]model.TestPipeline, error) {
	switch s.p.State {
	case "RETRY_QUEUED", "RETRY_SUBMITTING", "RETRY_UNCERTAIN":
		return []model.TestPipeline{s.p}, nil
	}
	return nil, nil
}
func (s *actionState) ClaimTestRetry(context.Context, string) (bool, error) {
	if !s.claim || s.p.State != "RETRY_QUEUED" {
		return false, nil
	}
	s.p.State = "RETRY_SUBMITTING"
	s.p.QuickRetries++
	s.p.RetrySubmittedAtMS = time.Now().UnixMilli()
	return true, nil
}
func (s *actionState) DeferTestRetry(_ context.Context, _ string, message string) error {
	s.p.State = "RETRY_UNCERTAIN"
	s.p.Error = message
	return nil
}
func (s *actionState) CompleteTestRetry(_ context.Context, _ string, _ model.PipelineObservation) error {
	s.p.State = "created"
	s.retryCompleted = true
	return nil
}
func (s *actionState) FailTestRetry(_ context.Context, _ string, message string) error {
	s.p.State = "failed"
	s.p.AnalysisRequired = true
	s.p.Error = message
	return nil
}
func (s *actionState) PendingTestEvidence(context.Context) ([]model.TestPipeline, error) {
	if s.p.State == "failed" && !model.PipelineFailureEvidenceReady(s.p.Jobs) {
		return []model.TestPipeline{s.p}, nil
	}
	return nil, nil
}
func (s *actionState) DeferTestEvidence(_ context.Context, _ string, message string) error {
	s.p.EvidenceError = message
	return nil
}
func (s *actionState) AttachTestEvidence(_ context.Context, _ string, o model.PipelineObservation) error {
	s.p.Jobs = o.Jobs
	s.evidenceAttached = true
	return nil
}

type actionClient struct {
	creates, finds, retries int
	err            error
	found          *model.PipelineObservation
	observed       model.PipelineObservation
	observeErr     error
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
func (c *actionClient) Retry(context.Context, model.TestPipeline) (model.PipelineObservation, error) {
	if c.state.p.State != "RETRY_SUBMITTING" {
		panic("retry POST before durable claim")
	}
	c.retries++
	if c.err != nil {
		return model.PipelineObservation{}, c.err
	}
	return model.PipelineObservation{ID: c.state.p.PipelineID, Ref: model.SeekDBTestRef, SHA: c.state.p.ConfigSHA, Status: "pending"}, nil
}
func (c *actionClient) Observe(context.Context, model.TestPipeline) (model.PipelineObservation, error) {
	return c.observed, c.observeErr
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

func TestActionHydratesFailedPipelineEvidenceWithoutGivingTokenToWorker(t *testing.T) {
	a, s, c := actionFixture()
	s.p = model.TestPipeline{ID: "evidence", Kind: model.SeekDBTestKind, State: "failed", PipelineID: 42, ConfigSHA: "config", Jobs: []model.PipelineJob{{ID: 7, Status: "failed"}}}
	c.observed = model.PipelineObservation{ID: 42, Status: "failed", Ref: model.SeekDBTestRef, SHA: "config", Jobs: []model.PipelineJob{{ID: 7, Status: "failed", LogCollected: true, LogExcerpt: "assertion failed"}}}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.evidenceAttached || !model.PipelineFailureEvidenceReady(s.p.Jobs) || s.p.Jobs[0].LogExcerpt != "assertion failed" {
		t.Fatal(s.p)
	}
}

func TestActionRetriesFailedJobsOnTheSamePipelineAfterDurableClaim(t *testing.T) {
	a, s, c := actionFixture()
	s.p = model.TestPipeline{ID: "retry", Kind: model.SeekDBTestKind, State: "RETRY_QUEUED", PipelineID: 42, ConfigSHA: strings.Repeat("b", 40), Jobs: []model.PipelineJob{{ID: 7, Status: "failed"}}}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.retries != 1 || !s.retryCompleted || s.p.State != "created" || s.p.QuickRetries != 1 {
		t.Fatal(s.p, c.retries)
	}
}

func TestActionNeverRepeatsAnUncertainRetryPost(t *testing.T) {
	a, s, c := actionFixture()
	s.p = model.TestPipeline{ID: "retry", Kind: model.SeekDBTestKind, State: "RETRY_QUEUED", PipelineID: 42, ConfigSHA: strings.Repeat("b", 40), Jobs: []model.PipelineJob{{ID: 7, Status: "failed"}}}
	c.err = errors.New("response lost")
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.retries != 1 || s.p.State != "RETRY_UNCERTAIN" {
		t.Fatal(s.p, c.retries)
	}
	c.err = nil
	c.observed = model.PipelineObservation{ID: 42, Status: "failed", Jobs: []model.PipelineJob{{ID: 7, Status: "failed"}}}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.retries != 1 || s.p.State != "RETRY_UNCERTAIN" {
		t.Fatal("uncertain mutation was repeated", s.p, c.retries)
	}
	c.observed = model.PipelineObservation{ID: 42, Status: "running"}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.retries != 1 || !s.retryCompleted {
		t.Fatal("read-only reconciliation did not finish original retry", s.p, c.retries)
	}
}
