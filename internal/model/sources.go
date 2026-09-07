package model

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Source settings contain references and filters, never credentials or shell
// commands. Provider implementations own authentication and transport.
type TaskSource struct {
	ID              string       `json:"source_id"`
	Kind            string       `json:"kind"`
	Name            string       `json:"name"`
	Enabled         bool         `json:"enabled"`
	Version         int64        `json:"version"`
	IntervalSeconds int          `json:"interval_seconds"`
	Config          SourceConfig `json:"config"`
}

type SourceConfig struct {
	WorkspaceID    string   `json:"workspace_id,omitempty"`
	WorkspaceSlug  string   `json:"workspace_slug,omitempty"`
	AssigneeID     string   `json:"assignee_id,omitempty"`
	IterationKey   string   `json:"iteration_key,omitempty"`
	IterationValue string   `json:"iteration_value,omitempty"`
	IgnoreLogins   []string `json:"ignore_logins,omitempty"`
}

// A target has its own durable cursor and retry schedule. One inaccessible PR
// must not stop other PRs or an unrelated source.
type SourceTarget struct {
	ID            string          `json:"target_id"`
	SourceID      string          `json:"source_id"`
	Entity        string          `json:"entity"`
	TaskID        string          `json:"task_id,omitempty"`
	Enabled       bool            `json:"enabled"`
	CreatedAtMS   int64           `json:"created_at_ms"`
	NextPollMS    int64           `json:"next_poll_ms"`
	LastSuccessMS int64           `json:"last_success_ms"`
	Error         string          `json:"error,omitempty"`
	Failures      int             `json:"failures"`
	Cursor        json.RawMessage `json:"-"`
	HeadSHA       string          `json:"head_sha,omitempty"`
}

type SourceEvent struct {
	ID          string `json:"event_id"`
	SourceID    string `json:"source_id"`
	TargetID    string `json:"target_id"`
	Key         string `json:"key"`
	Kind        string `json:"kind"`
	Entity      string `json:"entity"`
	TaskID      string `json:"task_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Message     string `json:"message"`
	HeadSHA     string `json:"head_sha,omitempty"`
	URL         string `json:"url,omitempty"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	CreatedAtMS int64  `json:"created_at_ms"`
}

type SourceReview struct {
	TargetID     string `json:"target_id"`
	HeadSHA      string `json:"head_sha"`
	RoleID       string `json:"role_id"`
	TaskID       string `json:"task_id"`
	State        string `json:"state"`
	FeedbackSent bool   `json:"feedback_sent"`
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var sourceKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,199}$`)
var CommitSHA = regexp.MustCompile(`^[a-f0-9]{40,64}$`)

// Only github.com is accepted in v1. Host adapters can be added explicitly;
// user-controlled URLs must never become arbitrary authenticated HTTP calls.
func ParseGitHubPR(raw string) (owner, repo string, number int, canonical string, err error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e == nil && u.Scheme == "https" && strings.EqualFold(u.Host, "github.com") && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 4 && parts[2] == "pull" && repoPart.MatchString(parts[0]) && repoPart.MatchString(parts[1]) {
			n, e := strconv.Atoi(parts[3])
			if e == nil && n > 0 && n < 1000000000 {
				return parts[0], parts[1], n, fmt.Sprintf("https://github.com/%s/%s/pull/%d", strings.ToLower(parts[0]), strings.ToLower(parts[1]), n), nil
			}
		}
	}
	return "", "", 0, "", fmt.Errorf("%w: 请填写完整的 https://github.com/owner/repo/pull/123 地址（不含查询参数）", ErrValidation)
}

func ValidateTaskSource(s TaskSource) error {
	if !sourceKey.MatchString(s.ID) || strings.TrimSpace(s.Name) == "" || len(s.Name) > 200 || s.IntervalSeconds < 5 || s.IntervalSeconds > 86400 {
		return fmt.Errorf("%w: 任务源名称必填，轮询间隔为 5～86400 秒", ErrValidation)
	}
	switch s.Kind {
	case "github":
		if len(s.Config.IgnoreLogins) > 50 {
			return fmt.Errorf("%w: 最多配置 50 个忽略账号", ErrValidation)
		}
		for _, login := range s.Config.IgnoreLogins {
			if len(login) > 100 || strings.ContainsAny(login, "\r\n\x00") {
				return fmt.Errorf("%w: 无效忽略账号", ErrValidation)
			}
		}
	case "antmultica":
		c := s.Config
		if !sourceKey.MatchString(c.WorkspaceID) || !sourceKey.MatchString(c.WorkspaceSlug) || !sourceKey.MatchString(c.AssigneeID) || strings.TrimSpace(c.IterationKey) == "" || len(c.IterationKey) > 200 || strings.ContainsAny(c.IterationKey, "\r\n\x00") || strings.TrimSpace(c.IterationValue) == "" || len(c.IterationValue) > 200 {
			return fmt.Errorf("%w: AntMultica 必须指定工作区、精确指派人、迭代字段和值", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: 不支持的任务源类型", ErrValidation)
	}
	return nil
}
