package taskaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/tasksource"
)

type PublicationStore interface {
	Maintenance(context.Context) (string, error)
	ReconcilePublications(context.Context) error
	ListPublications(context.Context, string) ([]model.Publication, error)
	ClaimPublication(context.Context, string) (model.Publication, error)
	FinishPublication(context.Context, model.Publication, string, string, string, string) error
}
type PublicationReceipt struct{ ID, URL string }
type PublicationError struct{ State, Message string }

// Returned only after a complete, successful read found no matching comment.
// The worker keeps reconciling; only an explicit human confirmation may retry.
type MissingPublicationError struct{}

func (*MissingPublicationError) Error() string {
	return "创建结果不明，完整查询尚未找到评论；只对账，不重复发布"
}

func (e *PublicationError) Error() string { return e.Message }

type Publisher interface {
	Publish(context.Context, model.Publication) (PublicationReceipt, error)
}
type Publications struct {
	Store      PublicationStore
	Publishers map[string]Publisher
}

func NewPublications(s PublicationStore) *Publications {
	gh := os.Getenv("WORK_ASSISTANT_GH_BINARY")
	if gh == "" {
		gh = "gh"
	}
	ant := os.Getenv("WORK_ASSISTANT_MULTICA_BINARY")
	if ant == "" {
		ant = "multica"
	}
	return &Publications{Store: s, Publishers: map[string]Publisher{"github": &GitHubComments{Run: tasksource.RunCLIInput, Binary: gh}, "antmultica": &AntMulticaComments{Run: tasksource.RunCLIInput, Binary: ant}}}
}
func (a *Publications) Tick(ctx context.Context) error {
	maintenance, err := a.Store.Maintenance(ctx)
	if err != nil || maintenance != "" {
		return err
	}
	if err = a.Store.ReconcilePublications(ctx); err != nil {
		return err
	}
	all, err := a.Store.ListPublications(ctx, "")
	if err != nil {
		return err
	}
	count := 0
	for _, p := range all {
		if p.State == "SYNCED" || p.State == "BLOCKED" || p.NextAttemptMS > time.Now().UnixMilli() {
			continue
		}
		if count >= 10 {
			return nil
		}
		count++
		publisher := a.Publishers[p.Platform]
		if publisher == nil {
			continue
		}
		p, err = a.Store.ClaimPublication(ctx, p.Key)
		if errors.Is(err, model.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		receipt, err := publisher.Publish(ctx, p)
		state, message := "SYNCED", ""
		if err != nil {
			state = "QUEUED"
			message = "平台读取失败，稍后重试；请检查控制主机登录和网络"
			var actionErr *PublicationError
			if errors.As(err, &actionErr) {
				state = actionErr.State
				message = actionErr.Message
			}
			var missing *MissingPublicationError
			if errors.As(err, &missing) {
				state = "UNCERTAIN"
				message = missing.Error()
			}
			if p.State == "UNCERTAIN" && state == "QUEUED" {
				state = "UNCERTAIN"
			}
		}
		// Record even if the network deadline expired. Shutdown waits for this loop.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		finishErr := a.Store.FinishPublication(saveCtx, p, receipt.ID, receipt.URL, state, message)
		cancel()
		if finishErr != nil {
			return finishErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

type InputRunner func(context.Context, string, []byte, ...string) ([]byte, error)
type GitHubComments struct {
	Run    InputRunner
	Binary string
}
type githubComment struct {
	ID       int64  `json:"id"`
	Body     string `json:"body"`
	URL      string `json:"html_url"`
	IssueURL string `json:"issue_url"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

func blocked(message string) error         { return &PublicationError{"BLOCKED", message} }
func uncertain(message string) error       { return &PublicationError{"UNCERTAIN", message} }
func publicationMarker(body string) string { marker, _, _ := strings.Cut(body, "\n"); return marker }
func (g *GitHubComments) Publish(ctx context.Context, p model.Publication) (PublicationReceipt, error) {
	var receipt PublicationReceipt
	owner, repo, number, canonical, err := model.ParseGitHubPR(p.URL)
	if !p.Sticky {
		owner, repo, number, canonical, err = model.ParseGitHubIssue(p.URL)
	}
	if err != nil || canonical != p.URL || (p.Sticky && !model.CommitSHA.MatchString(p.HeadSHA)) {
		return receipt, blocked("仅允许已绑定 PR 评审或原 Issue 进展评论")
	}
	marker := publicationMarker(p.Body)
	prefix := "review"
	if !p.Sticky {
		prefix = "progress"
	}
	if marker != "<!-- work-assistant:"+prefix+":"+p.Key+" -->" {
		return receipt, blocked("评论标识无效")
	}
	base := fmt.Sprintf("repos/%s/%s", owner, repo)
	issueEndpoint := fmt.Sprintf("%s/issues/%d", base, number)
	call := func(method, endpoint string, body []byte) ([]byte, error) {
		args := []string{"api", "--hostname", "github.com", "--method", method, endpoint}
		if body != nil {
			args = append(args, "--input", "-")
		}
		return g.Run(ctx, g.Binary, body, args...)
	}
	raw, err := call("GET", "user", nil)
	if err != nil {
		return receipt, err
	}
	var actor struct {
		Login string `json:"login"`
	}
	if json.Unmarshal(raw, &actor) != nil || actor.Login == "" {
		return receipt, blocked("无法确认 GitHub 评论身份")
	}
	targetEndpoint := issueEndpoint
	if p.Sticky {
		targetEndpoint = fmt.Sprintf("%s/pulls/%d", base, number)
	}
	raw, err = call("GET", targetEndpoint, nil)
	if err != nil {
		return receipt, err
	}
	var pr struct {
		URL         string          `json:"html_url"`
		State       string          `json:"state"`
		PullRequest json.RawMessage `json:"pull_request"`
		Head        struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if json.Unmarshal(raw, &pr) != nil || !strings.EqualFold(pr.URL, p.URL) || (p.Sticky && pr.Head.SHA == "") || (!p.Sticky && len(pr.PullRequest) > 0) {
		return receipt, blocked("PR/Issue 身份响应不一致")
	}
	// A raced new head is not a transport failure. Return QUEUED so projection
	// replaces the obsolete desired version on the next pass.
	valid := func(c githubComment) bool {
		return c.ID > 0 && strings.EqualFold(c.User.Login, actor.Login) && strings.EqualFold(c.IssueURL, "https://api.github.com/"+issueEndpoint) && publicationMarker(c.Body) == marker && strings.EqualFold(c.URL, fmt.Sprintf("%s#issuecomment-%d", p.URL, c.ID))
	}
	var found *githubComment
	if p.RemoteID != "" {
		n, e := strconv.ParseInt(p.RemoteID, 10, 64)
		if e != nil || n <= 0 {
			return receipt, blocked("已存评论 ID 无效")
		}
		raw, err = call("GET", base+"/issues/comments/"+p.RemoteID, nil)
		if err != nil {
			return receipt, err
		}
		var c githubComment
		if json.Unmarshal(raw, &c) != nil || c.ID != n || !valid(c) {
			return receipt, blocked("评论归属、作者或标识改变，停止覆盖")
		}
		found = &c
	} else {
		for page := 1; page <= 100; page++ {
			raw, err = call("GET", fmt.Sprintf("%s/comments?per_page=100&page=%d", issueEndpoint, page), nil)
			if err != nil {
				return receipt, err
			}
			var comments []githubComment
			if json.Unmarshal(raw, &comments) != nil || comments == nil {
				return receipt, blocked("评论分页响应无效")
			}
			for _, c := range comments {
				if !valid(c) {
					continue
				}
				if found != nil {
					return receipt, blocked("发现多个相同标识评论，需核对后继续")
				}
				copy := c
				found = &copy
			}
			if len(comments) < 100 {
				break
			}
			if page == 100 {
				return receipt, blocked("评论超过核对上限，未尝试重复创建")
			}
		}
	}
	if found != nil {
		receipt = PublicationReceipt{strconv.FormatInt(found.ID, 10), found.URL}
		if found.Body == p.Body {
			return receipt, nil
		}
		// Unknown comment contents are not safe to overwrite, including manual edits.
		if p.AppliedBody == "" || found.Body != p.AppliedBody {
			return receipt, blocked("固定评论已被外部修改，保留现有内容，需人工核对")
		}
	} else if p.State == "UNCERTAIN" {
		return receipt, &MissingPublicationError{}
	}
	if p.Sticky && pr.State != "open" {
		return receipt, blocked("PR 已关闭，未发布新的评审结论")
	}
	if p.Sticky && pr.Head.SHA != p.HeadSHA {
		return receipt, &PublicationError{"QUEUED", "PR 版本已变化，等待最新版本评审"}
	}
	body, _ := json.Marshal(map[string]string{"body": p.Body})
	method, endpoint := "POST", issueEndpoint+"/comments"
	if found != nil {
		method, endpoint = "PATCH", base+"/issues/comments/"+receipt.ID
	}
	raw, err = call(method, endpoint, body)
	if err != nil {
		return receipt, uncertain("GitHub 写入响应不明，正在核对固定评论")
	}
	var c githubComment
	if json.Unmarshal(raw, &c) != nil || !valid(c) || c.Body != p.Body {
		return receipt, uncertain("GitHub 返回未确认目标内容，正在核对固定评论")
	}
	if found != nil && c.ID != found.ID {
		return receipt, uncertain("GitHub 更新返回了不同评论 ID，需核对")
	}
	return PublicationReceipt{strconv.FormatInt(c.ID, 10), c.URL}, nil
}

type AntMulticaComments struct {
	Run    InputRunner
	Binary string
}

var uuidID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,99}$`)

type antComment struct {
	ID         string `json:"id"`
	IssueID    string `json:"issue_id"`
	Content    string `json:"content"`
	AuthorID   string `json:"author_id"`
	AuthorType string `json:"author_type"`
}

func (a *AntMulticaComments) Publish(ctx context.Context, p model.Publication) (PublicationReceipt, error) {
	var receipt PublicationReceipt
	marker := publicationMarker(p.Body)
	if p.Sticky || !uuidID.MatchString(p.WorkspaceID) || !uuidID.MatchString(p.IssueID) || !uuidID.MatchString(p.ActorID) || marker != "<!-- work-assistant:progress:"+p.Key+" -->" {
		return receipt, blocked("工单回写绑定无效")
	}
	call := func(input []byte, args ...string) ([]byte, error) {
		all := append([]string{"--server-url", "https://antmultica.alipay.com", "--workspace-id", p.WorkspaceID}, args...)
		return a.Run(ctx, a.Binary, input, all...)
	}
	raw, err := call(nil, "issue", "get", p.IssueID, "--output", "json")
	if err != nil {
		return receipt, err
	}
	var issue struct {
		ID        string `json:"id"`
		Workspace string `json:"workspace_id"`
	}
	if json.Unmarshal(raw, &issue) != nil || issue.ID != p.IssueID || issue.Workspace != p.WorkspaceID {
		return receipt, blocked("AntMultica 工单不在绑定工作区")
	}
	raw, err = call(nil, "issue", "comment", "list", p.IssueID, "--roots-only", "--full", "--output", "json")
	if err != nil {
		return receipt, err
	}
	var comments []antComment
	if json.Unmarshal(raw, &comments) != nil || comments == nil {
		return receipt, blocked("AntMultica 评论列表无法完整核对")
	}
	matches := 0
	for _, c := range comments {
		if c.IssueID == p.IssueID && c.AuthorID == p.ActorID && c.AuthorType == "member" && publicationMarker(c.Content) == marker {
			matches++
			if c.Content != p.Body {
				return receipt, blocked("工单回写标识内容冲突")
			}
			receipt = PublicationReceipt{c.ID, p.URL}
		}
	}
	if matches > 1 {
		return receipt, blocked("工单有多个相同回写标识")
	}
	if matches == 1 {
		return receipt, nil
	}
	if p.State == "UNCERTAIN" {
		return receipt, &MissingPublicationError{}
	}
	raw, err = call([]byte(p.Body), "issue", "comment", "add", p.IssueID, "--content-stdin", "--output", "json")
	if err != nil {
		return receipt, uncertain("AntMultica 写入响应不明，正在核对工单评论")
	}
	var c antComment
	if json.Unmarshal(raw, &c) != nil || c.ID == "" || c.IssueID != p.IssueID || c.AuthorID != p.ActorID || c.AuthorType != "member" || c.Content != p.Body {
		return receipt, uncertain("AntMultica 返回未确认回写，正在核对")
	}
	return PublicationReceipt{c.ID, p.URL}, nil
}
