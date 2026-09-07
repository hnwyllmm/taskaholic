// Package taskaction executes explicit, durable Agent requests. Task sources
// never call this package; they only observe state and append inbox events.
package taskaction

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

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
}
type PipelineActions struct {
	Store     PipelineStore
	Executors map[string]gitlabci.Executor
	LatestPR  func(context.Context, string) (head, state string, err error)
}

func NewPipelines(s PipelineStore, executor gitlabci.Executor) *PipelineActions {
	gh := os.Getenv("WORK_ASSISTANT_GH_BINARY")
	if gh == "" {
		gh = "gh"
	}
	return &PipelineActions{Store: s, Executors: map[string]gitlabci.Executor{model.SeekDBTestKind: executor}, LatestPR: func(ctx context.Context, pr string) (string, string, error) {
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
	return nil
}
