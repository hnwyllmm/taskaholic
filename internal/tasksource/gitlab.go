package tasksource

import (
	"context"
	"encoding/json"
	"fmt"

	"work-assistant/internal/gitlabci"
	"work-assistant/internal/model"
)

type GitLab struct {
	Read   gitlabci.Reader
	Lookup func(context.Context, string) (model.TestPipeline, error)
}

func (g *GitLab) Poll(ctx context.Context, source model.TaskSource, target model.SourceTarget) (PollResult, error) {
	p, err := g.Lookup(ctx, target.TestRequestID)
	if err != nil {
		return PollResult{}, err
	}
	if p.PollTargetID != target.ID || p.TaskID != target.TaskID || p.URL != target.Entity {
		return PollResult{}, fmt.Errorf("pipeline 轮询目标与任务登记不匹配")
	}
	observation, err := g.Read.Observe(ctx, p)
	if err != nil {
		return PollResult{}, err
	}
	var previous struct {
		Status string `json:"status"`
		Seq    int    `json:"seq"`
	}
	if err = json.Unmarshal(target.Cursor, &previous); err != nil {
		return PollResult{}, err
	}
	evidenceChanged := observation.Status == "failed" && digest(p.Jobs) != digest(observation.Jobs)
	closed := model.PipelineFinished(observation.Status)
	if observation.Status == "failed" && !model.PipelineFailureEvidenceReady(observation.Jobs) {
		// Keep collecting in the background when the pipeline is terminal but
		// its actionable logs are not yet available. This never reruns a test.
		closed = false
	}
	result := PollResult{HeadSHA: p.HeadSHA, Closed: closed, Cursor: target.Cursor}
	if previous.Status != observation.Status || evidenceChanged {
		previous.Status = observation.Status
		previous.Seq++
		result.Events = []model.SourceEvent{{Key: fmt.Sprintf("pipeline:%d:%d:%s", p.PipelineID, previous.Seq, observation.Status), Kind: "gitlab.pipeline", Entity: p.URL, HeadSHA: p.HeadSHA, URL: p.URL, Message: fmt.Sprintf("GitLab pipeline #%d：%s；被测 PR SHA：%s", p.PipelineID, observation.Status, p.HeadSHA), Pipeline: &observation}}
		result.Cursor, _ = json.Marshal(previous)
	}
	return result, nil
}
