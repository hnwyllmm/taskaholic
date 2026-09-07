// Package gitlabci is a credential-isolated client for the approved test suite.
// Create is only used by the action executor. Pollers receive the Reader surface.
package gitlabci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"work-assistant/internal/model"
)

type Reader interface {
	Observe(context.Context, model.TestPipeline) (model.PipelineObservation, error)
}
type Executor interface {
	Create(context.Context, model.TestPipeline) (model.PipelineObservation, error)
	Find(context.Context, model.TestPipeline) (*model.PipelineObservation, error)
}

type Client struct {
	HTTP  *http.Client
	Token func() (string, error)
	base  string // fixed in production, injectable only in this package's tests
}

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("GitLab API HTTP %d（响应正文不写入任务或日志）", e.Status)
}

type RejectedError struct{ Message string }

func (e *RejectedError) Error() string { return e.Message }
func DefinitelyRejected(err error) bool {
	var rejected *RejectedError
	if errors.As(err, &rejected) {
		return true
	}
	var h *HTTPError
	if errors.As(err, &h) {
		switch h.Status {
		case 400, 401, 403, 404, 405, 422:
			return true
		}
	}
	return false
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second}, base: model.SeekDBTestHost, Token: func() (string, error) {
		file := os.Getenv("WORK_ASSISTANT_GITLAB_TOKEN_FILE")
		if file == "" {
			home, _ := os.UserHomeDir()
			file = filepath.Join(home, ".config", "work-assistant", "gitlab.token")
		}
		if raw, err := os.ReadFile(file); err == nil && strings.TrimSpace(string(raw)) != "" {
			return strings.TrimSpace(string(raw)), nil
		}
		for _, key := range []string{"GITLAB_TOKEN", "GITLAB_PRIVATE_TOKEN"} {
			if token := strings.TrimSpace(os.Getenv(key)); token != "" {
				return token, nil
			}
		}
		return "", &RejectedError{"GitLab 认证未配置，请在控制主机配置受保护的 gitlab.token 文件"}
	}}
}

// Check verifies the exact project and branch without starting any pipeline.
func (c *Client) Check(ctx context.Context) (map[string]any, error) {
	var project struct {
		ID   int64  `json:"id"`
		Path string `json:"path_with_namespace"`
	}
	if _, err := c.call(ctx, "GET", "", nil, &project); err != nil {
		return nil, err
	}
	if project.ID <= 0 || project.Path != model.SeekDBTestProject {
		return nil, fmt.Errorf("GitLab project identity mismatch")
	}
	var branch struct {
		Name   string `json:"name"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if _, err := c.call(ctx, "GET", "/repository/branches/"+url.PathEscape(model.SeekDBTestRef), nil, &branch); err != nil {
		return nil, err
	}
	if branch.Name != model.SeekDBTestRef || !model.CommitSHA.MatchString(branch.Commit.ID) {
		return nil, fmt.Errorf("GitLab test branch identity mismatch")
	}
	return map[string]any{"project_id": project.ID, "project": project.Path, "ref": branch.Name, "config_sha": branch.Commit.ID, "read_only": true}, nil
}

func (c *Client) call(ctx context.Context, method, path string, body any, result any) (http.Header, error) {
	token, err := c.Token()
	if err != nil {
		return nil, err
	}
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v4/projects/"+url.PathEscape(model.SeekDBTestProject)+path, input)
	if err != nil {
		return nil, fmt.Errorf("invalid GitLab API request")
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	client := *c.HTTP
	// A redirect must never forward the credential or repeat a POST elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitLab 请求未取得确定响应（请检查连接，提交可能已成功）")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.Header, &HTTPError{response.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 {
		return nil, fmt.Errorf("GitLab 响应不完整或超过 2 MiB")
	}
	if err = json.Unmarshal(raw, result); err != nil {
		return nil, fmt.Errorf("GitLab 响应格式无效")
	}
	return response.Header, nil
}

type variable struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func variables(p model.TestPipeline) []variable {
	return []variable{{"SEEKDB_SOURCE", p.HeadSHA}, {"JOBS", "all"}, {"RUN_PROFILE", "1"}, {"WORK_ASSISTANT_REQUEST_ID", p.ID}}
}
func matches(vars []variable, p model.TestPipeline) bool {
	for _, want := range variables(p) {
		count := 0
		for _, got := range vars {
			if got.Key == want.Key {
				if got.Value != want.Value {
					return false
				}
				count++
			}
		}
		if count != 1 {
			return false
		}
	}
	return true
}
func validRequest(p model.TestPipeline) bool {
	owner, repo, _, _, err := model.ParseGitHubPR(p.PRURL)
	return err == nil && strings.ToLower(owner+"/"+repo) == "oceanbase/seekdb" && p.Kind == model.SeekDBTestKind && len(p.HeadSHA) == 40 && model.CommitSHA.MatchString(p.HeadSHA) && p.ID != ""
}
func validate(p model.PipelineObservation) error {
	if p.ID <= 0 || p.Ref != model.SeekDBTestRef || len(p.SHA) != 40 || !model.CommitSHA.MatchString(p.SHA) {
		return fmt.Errorf("GitLab pipeline identity/ref is invalid")
	}
	switch p.Status {
	case "created", "waiting_for_resource", "preparing", "waiting_for_callback", "pending", "running", "success", "failed", "canceling", "canceled", "skipped", "manual", "scheduled":
		return nil
	}
	return fmt.Errorf("GitLab pipeline status is unknown; not treated as passed")
}
func (c *Client) Create(ctx context.Context, p model.TestPipeline) (model.PipelineObservation, error) {
	var out model.PipelineObservation
	if !validRequest(p) {
		return out, &RejectedError{"测试申请不符合已授权仓库和 SHA 约束"}
	}
	_, err := c.call(ctx, "POST", "/pipeline", map[string]any{"ref": model.SeekDBTestRef, "variables": variables(p)}, &out)
	if err == nil {
		err = validate(out)
	}
	out.URL = model.PipelineURL(out.ID)
	return out, err
}
func (c *Client) Find(ctx context.Context, p model.TestPipeline) (*model.PipelineObservation, error) {
	if !validRequest(p) {
		return nil, fmt.Errorf("invalid reconciliation request")
	}
	since := time.UnixMilli(p.SubmittedAtMS).Add(-time.Minute)
	query := url.Values{"ref": {model.SeekDBTestRef}, "source": {"api"}, "created_after": {since.UTC().Format(time.RFC3339)}, "created_before": {since.Add(15 * time.Minute).UTC().Format(time.RFC3339)}, "per_page": {"100"}, "order_by": {"id"}, "sort": {"asc"}}
	var found *model.PipelineObservation
	for page := 1; page <= 5; page++ {
		query.Set("page", strconv.Itoa(page))
		var items []model.PipelineObservation
		h, err := c.call(ctx, "GET", "/pipelines?"+query.Encode(), nil, &items)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			var vars []variable
			if _, err = c.call(ctx, "GET", fmt.Sprintf("/pipelines/%d/variables", item.ID), nil, &vars); err != nil {
				return nil, err
			}
			if !matches(vars, p) {
				continue
			}
			if err = validate(item); err != nil {
				return nil, err
			}
			if found != nil {
				return nil, fmt.Errorf("发现多个匹配的 pipeline，请人工核查，不会自动再次发起")
			}
			item.URL = model.PipelineURL(item.ID)
			copy := item
			found = &copy
		}
		if h.Get("X-Next-Page") == "" && len(items) < 100 {
			return found, nil
		}
	}
	return nil, fmt.Errorf("pipeline 对账超过分页上限，保留申请等待人工核查")
}
func (c *Client) Observe(ctx context.Context, p model.TestPipeline) (model.PipelineObservation, error) {
	var out struct {
		model.PipelineObservation
		ProjectID int64 `json:"project_id"`
	}
	if !validRequest(p) || p.PipelineID <= 0 {
		return out.PipelineObservation, fmt.Errorf("invalid pipeline watch")
	}
	_, err := c.call(ctx, "GET", fmt.Sprintf("/pipelines/%d", p.PipelineID), nil, &out)
	if err != nil {
		return out.PipelineObservation, err
	}
	if err = validate(out.PipelineObservation); err != nil {
		return out.PipelineObservation, err
	}
	if out.ID != p.PipelineID || out.SHA != p.ConfigSHA {
		return out.PipelineObservation, fmt.Errorf("pipeline identity/config revision changed")
	}
	var vars []variable
	if _, err = c.call(ctx, "GET", fmt.Sprintf("/pipelines/%d/variables", p.PipelineID), nil, &vars); err != nil {
		return out.PipelineObservation, err
	}
	if !matches(vars, p) {
		return out.PipelineObservation, fmt.Errorf("pipeline 的被测 SHA / 测试范围 / 申请标识不匹配，不作为验收证据")
	}
	out.URL = model.PipelineURL(out.ID)
	if model.PipelineFinished(out.Status) && out.Status != "success" {
		// Parent pipelines use strategy:depend. Include failures in their child
		// pipelines, not just the parent's uninformative dispatch failure.
		out.Jobs, err = c.failureJobs(ctx, out.ID, out.ProjectID, 0, map[int64]bool{})
		if err != nil {
			out.Jobs = append(out.Jobs, model.PipelineJob{Name: "作业明细暂不可用，请查看 pipeline 页面", Status: "unknown", FailureReason: err.Error(), URL: out.URL})
		}
	}
	return out.PipelineObservation, nil
}
func (c *Client) failureJobs(ctx context.Context, pipelineID, projectID int64, depth int, seen map[int64]bool) ([]model.PipelineJob, error) {
	if seen[pipelineID] || depth > 2 || len(seen) >= 10 {
		return nil, fmt.Errorf("子流水线超过安全采集范围，请查看 GitLab 页面")
	}
	seen[pipelineID] = true
	var jobs []model.PipelineJob
	for page := 1; page <= 5; page++ {
		var batch []model.PipelineJob
		h, err := c.call(ctx, "GET", fmt.Sprintf("/pipelines/%d/jobs?scope[]=failed&scope[]=canceled&per_page=100&page=%d", pipelineID, page), nil, &batch)
		if err != nil {
			return jobs, err
		}
		for _, j := range batch {
			j.URL = fmt.Sprintf("%s/%s/-/jobs/%d", model.SeekDBTestHost, model.SeekDBTestProject, j.ID)
			j.Name = short(j.Name, 200)
			j.FailureReason = short(j.FailureReason, 200)
			jobs = append(jobs, j)
			if len(jobs) >= 30 {
				return jobs, fmt.Errorf("只展示前 30 个失败作业，完整列表见 GitLab")
			}
		}
		if h.Get("X-Next-Page") == "" && len(batch) < 100 {
			break
		}
		if page == 5 {
			return jobs, fmt.Errorf("失败作业列表超过分页上限")
		}
	}
	var bridges []struct {
		model.PipelineJob
		Downstream *struct {
			ID        int64 `json:"id"`
			ProjectID int64 `json:"project_id"`
		} `json:"downstream_pipeline"`
	}
	h, err := c.call(ctx, "GET", fmt.Sprintf("/pipelines/%d/bridges?per_page=100", pipelineID), nil, &bridges)
	if err != nil {
		return jobs, err
	}
	if h.Get("X-Next-Page") != "" || len(bridges) >= 100 {
		return jobs, fmt.Errorf("子流水线列表超过采集上限")
	}
	for _, b := range bridges {
		if b.Downstream == nil {
			continue
		}
		if projectID <= 0 || b.Downstream.ProjectID != projectID {
			return jobs, fmt.Errorf("跨项目子流水线未授权自动采集")
		}
		extra, err := c.failureJobs(ctx, b.Downstream.ID, projectID, depth+1, seen)
		jobs = append(jobs, extra...)
		if len(jobs) > 30 {
			jobs = jobs[:30]
		}
		if err != nil {
			return jobs, err
		}
	}
	return jobs, nil
}
func short(value string, max int) string {
	r := []rune(value)
	if len(r) > max {
		return string(r[:max])
	}
	return value
}
