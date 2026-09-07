// Package tasksource contains replaceable, read-only event producers.
// Providers never choose an agent or start a Run; the durable inbox does that.
package tasksource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

type PollResult struct {
	Cursor     json.RawMessage
	Events     []model.SourceEvent
	HeadSHA    string
	Closed     bool
	NextPollMS int64
}
type Provider interface {
	Poll(context.Context, model.TaskSource, model.SourceTarget) (PollResult, error)
}
type Runner func(context.Context, string, ...string) ([]byte, error)
type RetryError struct {
	Message string
	At      time.Time
	Global  bool
}

func (e *RetryError) Error() string { return e.Message }

// No shell interpolation or credential extraction. Existing CLI auth stores
// remain on the host; stderr is intentionally not persisted (may contain secrets).
func RunCLI(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	allowed := map[string]bool{"HOME": true, "USER": true, "PATH": true, "LANG": true, "LC_ALL": true, "TMPDIR": true, "TMP": true, "TEMP": true, "XDG_CONFIG_HOME": true, "GH_CONFIG_DIR": true, "GH_TOKEN": true, "GITHUB_TOKEN": true, "MULTICA_TOKEN": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = 2 * time.Second
	out := &limitedBuffer{limit: 8 * 1024 * 1024}
	cmd.Stdout = out
	err := cmd.Run()
	if out.overflow {
		return nil, fmt.Errorf("provider response exceeds 8 MiB")
	}
	return out.Bytes(), err
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.overflow = true
		return 0, fmt.Errorf("output limit")
	}
	return b.Buffer.Write(p)
}

type Engine struct {
	Store     *store.Store
	Providers map[string]Provider
}

func New(state *store.Store) *Engine {
	gh := os.Getenv("WORK_ASSISTANT_GH_BINARY")
	if gh == "" {
		gh = "gh"
	}
	multica := os.Getenv("WORK_ASSISTANT_MULTICA_BINARY")
	if multica == "" {
		multica = "multica"
	}
	return &Engine{Store: state, Providers: map[string]Provider{"github": &GitHub{Run: RunCLI, Binary: gh}, "antmultica": &AntMultica{Run: RunCLI, Binary: multica}}}
}
func (e *Engine) Tick(ctx context.Context) error {
	maintenance, err := e.Store.Maintenance(ctx)
	if err != nil {
		return err
	}
	if maintenance != "" {
		return nil
	}
	if err := e.Store.CollectSourceReviews(ctx); err != nil {
		return err
	}
	if err := e.Store.ProcessSourceEvents(ctx); err != nil {
		return err
	}
	sources, err := e.Store.ListTaskSources(ctx)
	if err != nil {
		return err
	}
	config := map[string]model.TaskSource{}
	for _, s := range sources {
		config[s.ID] = s
	}
	targets, err := e.Store.ListSourceTargets(ctx)
	if err != nil {
		return err
	}
	blocked := map[string]bool{}
	count := 0
	for _, t := range targets {
		s := config[t.SourceID]
		if !s.Enabled || !t.Enabled || t.NextPollMS > time.Now().UnixMilli() || blocked[s.Kind] {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p := e.Providers[s.Kind]
		var result PollResult
		if p == nil {
			err = fmt.Errorf("任务源插件 %s 未安装", s.Kind)
		} else {
			pollCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			result, err = p.Poll(pollCtx, s, t)
			cancel()
		}
		if err == nil {
			next := time.Now().Add(time.Duration(s.IntervalSeconds) * time.Second).UnixMilli()
			if result.NextPollMS > next {
				next = result.NextPollMS
			}
			err = e.Store.CommitSourcePoll(ctx, s, t, result.Cursor, result.HeadSHA, result.Events, next, result.Closed)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := time.Duration(1<<min(t.Failures, 8)) * 30 * time.Second
			retry := time.Now().Add(delay)
			if r, ok := err.(*RetryError); ok {
				if r.At.After(retry) {
					retry = r.At
				}
				if r.Global {
					blocked[s.Kind] = true
					// Persist the shared rate-limit gate across daemon restarts.
					for _, other := range targets {
						if config[other.SourceID].Kind == s.Kind && other.ID != t.ID {
							if x := e.Store.FailSourcePoll(ctx, other.ID, r.Message, retry.UnixMilli()); x != nil {
								return x
							}
						}
					}
				}
			}
			if x := e.Store.FailSourcePoll(ctx, t.ID, err.Error(), retry.UnixMilli()); x != nil {
				return x
			}
		}
		count++
		if count >= 20 {
			break
		}
	}
	return e.Store.ProcessSourceEvents(ctx)
}

func digest(value any) string {
	raw, _ := json.Marshal(value)
	return hash(raw)
}
