// Package taskaction executes explicit, durable Agent requests. Task sources
// never call this package; they only observe state and append inbox events.
package taskaction

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"work-assistant/internal/gitlabci"
	"work-assistant/internal/model"
	"work-assistant/internal/tasksource"
)

type PipelineStore interface {
	Maintenance(context.Context) (string, error)
	PendingTestActions(context.Context) ([]model.TestPipeline, error)
	ClaimTestAction(context.Context, string) (bool, error)
	DeferTestAction(context.Context, string, string) error
	FailTestAction(context.Context, string, string, string) error
	AttachTestPipeline(context.Context, string, model.PipelineObservation) error
	PendingTestRetries(context.Context) ([]model.TestPipeline, error)
	ClaimTestRetry(context.Context, string) (bool, error)
	DeferTestRetry(context.Context, string, string) error
	CompleteTestRetry(context.Context, string, model.PipelineObservation) error
	FailTestRetry(context.Context, string, string) error
	PendingTestEvidence(context.Context) ([]model.TestPipeline, error)
	DeferTestEvidence(context.Context, string, string) error
	AttachTestEvidence(context.Context, string, model.PipelineObservation) error
}
type PipelineActions struct {
	Store     PipelineStore
	Executors map[string]gitlabci.Executor
	Retriers  map[string]gitlabci.Retrier
	Readers   map[string]gitlabci.Reader
	LatestPR  func(context.Context, string) (head, state string, err error)
}

func NewPipelines(s PipelineStore, executor gitlabci.Executor) *PipelineActions {
	gh := os.Getenv("WORK_ASSISTANT_GH_BINARY")
	if gh == "" {
		gh = "gh"
	}
	readers := map[string]gitlabci.Reader{}
	retriers := map[string]gitlabci.Retrier{}
	if reader, ok := executor.(gitlabci.Reader); ok {
		readers[model.SeekDBTestKind] = reader
	}
	if retrier, ok := executor.(gitlabci.Retrier); ok {
		retriers[model.SeekDBTestKind] = retrier
	}
	return &PipelineActions{Store: s, Executors: map[string]gitlabci.Executor{model.SeekDBTestKind: executor}, Retriers: retriers, Readers: readers, LatestPR: func(ctx context.Context, pr string) (string, string, error) {
		owner, repo, number, _, err := model.ParseGitHubPR(pr)
		if err != nil {
			return "", "", err
		}
		raw, err := tasksource.RunCLI(ctx, gh, "api", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number))
		if err != nil {
			return "", "", fmt.Errorf("无法核实 GitHub PR 最新 SHA，尚未发起测试")
		}
		var info struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
			State string `json:"state"`
		}
		if err = json.Unmarshal(raw, &info); err != nil || len(info.Head.SHA) != 40 || !model.CommitSHA.MatchString(info.Head.SHA) {
			return "", "", fmt.Errorf("GitHub PR 版本响应无效")
		}
		return info.Head.SHA, info.State, nil
	}}
}
func (a *PipelineActions) Tick(ctx context.Context) error {
	maintenance, err := a.Store.Maintenance(ctx)
	if err != nil || maintenance != "" {
		return err
	}
	if err = a.tickRetries(ctx); err != nil {
		return err
	}
	pending, err := a.Store.PendingTestActions(ctx)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		executor := a.Executors[p.Kind]
		if executor == nil {
			if err = a.Store.FailTestAction(ctx, p.ID, "ERROR", "测试执行插件未安装"); err != nil {
				return err
			}
			continue
		}
		if p.State != "QUEUED" {
			found, err := executor.Find(ctx, p)
			if err == nil && found != nil {
				if err = a.Store.AttachTestPipeline(ctx, p.ID, *found); err != nil {
					return err
				}
				continue
			}
			message := "提交结果尚未确认，正在按申请标识查询 GitLab"
			if err != nil {
				message = err.Error()
			}
			if err = a.Store.DeferTestAction(ctx, p.ID, message); err != nil {
				return err
			}
			continue
		}
		head, state, err := a.LatestPR(ctx, p.PRURL)
		if err != nil {
			if err = a.Store.DeferTestAction(ctx, p.ID, err.Error()); err != nil {
				return err
			}
			continue
		}
		if head != p.HeadSHA || state != "open" {
			if err = a.Store.FailTestAction(ctx, p.ID, "SUPERSEDED", "PR 已关闭或最新 SHA 已变化；未发起旧版本测试"); err != nil {
				return err
			}
			continue
		}
		claimed, err := a.Store.ClaimTestAction(ctx, p.ID)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		observation, err := executor.Create(ctx, p)
		if err != nil {
			if gitlabci.DefinitelyRejected(err) {
				err = a.Store.FailTestAction(ctx, p.ID, "ERROR", err.Error())
			} else {
				err = a.Store.DeferTestAction(ctx, p.ID, err.Error())
			}
			if err != nil {
				return err
			}
			continue
		}
		if err = a.Store.AttachTestPipeline(ctx, p.ID, observation); err != nil {
			return err
		}
	}
	evidence, err := a.Store.PendingTestEvidence(ctx)
	if err != nil {
		return err
	}
	for _, p := range evidence {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reader := a.Readers[p.Kind]
		if reader == nil {
			if err = a.Store.DeferTestEvidence(ctx, p.ID, "GitLab 失败日志读取插件未安装"); err != nil {
				return err
			}
			continue
		}
		observation, readErr := reader.Observe(ctx, p)
		if readErr != nil {
			if err = a.Store.DeferTestEvidence(ctx, p.ID, readErr.Error()); err != nil {
				return err
			}
			continue
		}
		if !model.PipelineFailureEvidenceReady(observation.Jobs) {
			message := "GitLab 失败日志尚未全部取得"
			for _, job := range observation.Jobs {
				if job.LogCollectError != "" {
					message = job.LogCollectError
					break
				}
			}
			if err = a.Store.DeferTestEvidence(ctx, p.ID, message); err != nil {
				return err
			}
			continue
		}
		if err = a.Store.AttachTestEvidence(ctx, p.ID, observation); err != nil {
			return err
		}
	}
	return nil
}

func (a *PipelineActions) tickRetries(ctx context.Context) error {
	pending, err := a.Store.PendingTestRetries(ctx)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		retrier, reader := a.Retriers[p.Kind], a.Readers[p.Kind]
		if retrier == nil || reader == nil {
			if err = a.Store.FailTestRetry(ctx, p.ID, "测试插件不支持原 Pipeline 失败作业重试"); err != nil {
				return err
			}
			continue
		}
		if p.State != "RETRY_QUEUED" {
			observed, readErr := reader.Observe(ctx, p)
			if readErr == nil && retryObservationChanged(p, observed) {
				if err = a.Store.CompleteTestRetry(ctx, p.ID, observed); err != nil {
					return err
				}
				continue
			}
			if time.Now().UnixMilli()-p.RetrySubmittedAtMS >= 2*time.Minute.Milliseconds() {
				message := "失败作业重试提交两分钟后仍无法确认，已停止自动重试"
				if readErr != nil {
					message += "：" + readErr.Error()
				}
				if err = a.Store.FailTestRetry(ctx, p.ID, message); err != nil {
					return err
				}
				continue
			}
			message := "重试提交结果尚未反映到 Pipeline，继续只读对账"
			if readErr != nil {
				message = readErr.Error()
			}
			if err = a.Store.DeferTestRetry(ctx, p.ID, message); err != nil {
				return err
			}
			continue
		}
		claimed, claimErr := a.Store.ClaimTestRetry(ctx, p.ID)
		if claimErr != nil {
			return claimErr
		}
		if !claimed {
			continue
		}
		// Refresh the persisted claim: RetrySubmittedAtMS and QuickRetries were
		// written before the only mutating GitLab call.
		p.State = "RETRY_SUBMITTING"
		p.QuickRetries++
		observation, retryErr := retrier.Retry(ctx, p)
		if retryErr != nil {
			if gitlabci.DefinitelyRejected(retryErr) {
				err = a.Store.FailTestRetry(ctx, p.ID, retryErr.Error())
			} else {
				err = a.Store.DeferTestRetry(ctx, p.ID, retryErr.Error())
			}
			if err != nil {
				return err
			}
			continue
		}
		if err = a.Store.CompleteTestRetry(ctx, p.ID, observation); err != nil {
			return err
		}
	}
	return nil
}

func retryObservationChanged(p model.TestPipeline, observed model.PipelineObservation) bool {
	if observed.ID != p.PipelineID || observed.Status != "failed" {
		return observed.ID == p.PipelineID
	}
	before := make(map[int64]bool, len(p.Jobs))
	for _, job := range p.Jobs {
		before[job.ID] = true
	}
	if len(observed.Jobs) != len(p.Jobs) {
		return true
	}
	for _, job := range observed.Jobs {
		if !before[job.ID] {
			return true
		}
	}
	return false
}
