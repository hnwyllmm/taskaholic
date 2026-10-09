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
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// Retrier is separate from Executor so other test plugins do not gain this
// external mutation implicitly. A suite must explicitly support retrying an
// already verified pipeline without creating another full pipeline.
type Retrier interface {
	Retry(context.Context, model.TestPipeline) (model.PipelineObservation, error)
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

const (
	maxJobTraceBytes  = 16 * 1024 * 1024
	jobTraceTailRunes = 8 * 1024

	// Every retained mysqltest failure receives a trace collection attempt: the
	// final mysqltest failure count is a count of test cases in these logs, not
	// merely a count of GitLab jobs. Non-mysqltest logs are still sampled, since
	// the Agent's retry eligibility for those jobs is deliberately small.
	maxRecordedMySQLTestJobs    = 64
	maxRecordedOtherFailedJobs  = 12
	maxTracedOtherFailedJobs    = 3
	maxConcurrentTraceDownloads = 6
)

var (
	ansiEscape       = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	secretAssignment = regexp.MustCompile(`(?i)\b(private-token|authorization|access[_-]?token|api[_-]?key|password|passwd|secret)([ \t]*[:=][ \t]*)([^\s"']+)`)
	knownToken       = regexp.MustCompile(`\b(?:glpat-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]{20,})\b`)
)

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

func (c *Client) jobTrace(ctx context.Context, jobID int64) (string, bool, error) {
	if jobID <= 0 {
		return "", false, fmt.Errorf("invalid GitLab job identity")
	}
	token, err := c.Token()
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/v4/projects/"+url.PathEscape(model.SeekDBTestProject)+fmt.Sprintf("/jobs/%d/trace", jobID), nil)
	if err != nil {
		return "", false, fmt.Errorf("invalid GitLab trace request")
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Accept", "text/plain")
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("GitLab 作业日志暂时不可读（请检查连接）")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", false, &HTTPError{response.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxJobTraceBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("GitLab 作业日志读取不完整")
	}
	if len(raw) > maxJobTraceBytes {
		return "", false, fmt.Errorf("GitLab 作业日志超过 16 MiB 自动采集上限")
	}
	clean := sanitizeTrace(string(raw))
	runes := []rune(clean)
	truncated := len(runes) > jobTraceTailRunes
	if truncated {
		runes = runes[len(runes)-jobTraceTailRunes:]
		// Start at a complete line when possible; the omitted prefix is not
		// useful diagnostic context and may contain unrelated setup output.
		if newline := strings.IndexRune(string(runes), '\n'); newline >= 0 {
			runes = []rune(string(runes)[newline+1:])
		}
	}
	return strings.TrimSpace(string(runes)), truncated, nil
}

func sanitizeTrace(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.ReplaceAll(value, "\x00", "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = ansiEscape.ReplaceAllString(value, "")
	value = secretAssignment.ReplaceAllString(value, "$1$2[REDACTED]")
	return knownToken.ReplaceAllString(value, "[REDACTED]")
}

func (c *Client) collectFailureLogs(ctx context.Context, jobs []model.PipelineJob) {
	// Trace every retained mysqltest job.  This makes it possible for the
	// development Agent to count the failed mysqltest cases in each individual
	// result.  A small non-mysqltest sample is sufficient for the separate
	// three-job retry policy.
	selected := make([]int, 0, len(jobs))
	nonMySQL := 0
	for i, job := range jobs {
		if job.ID <= 0 {
			continue
		}
		if model.IsMySQLTestPipelineJob(job.Name) {
			selected = append(selected, i)
			continue
		}
		if nonMySQL < maxTracedOtherFailedJobs {
			selected = append(selected, i)
			nonMySQL++
			continue
		}
		jobs[i].LogCollected = true
		jobs[i].LogCollectError = "非 mysqltest 失败作业超过 3 个；已保留 GitLab 链接，mysqltest 日志均会单独采集"
	}
	if len(selected) == 0 {
		return
	}
	type traceResult struct {
		index     int
		excerpt   string
		truncated bool
		err       error
	}
	results := make(chan traceResult, len(selected))
	workers := maxConcurrentTraceDownloads
	if workers > len(selected) {
		workers = len(selected)
	}
	work := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range work {
				excerpt, truncated, err := c.jobTrace(ctx, jobs[index].ID)
				results <- traceResult{index: index, excerpt: excerpt, truncated: truncated, err: err}
			}
		}()
	}
	go func() {
		for _, index := range selected {
			work <- index
		}
		close(work)
		wait.Wait()
		close(results)
	}()
	for result := range results {
		if result.err != nil {
			jobs[result.index].LogCollectError = short(result.err.Error(), 200)
			continue
		}
		if strings.TrimSpace(result.excerpt) == "" {
			// A terminal failure with an empty trace gives the Agent no material
			// from which to count mysqltest cases or assess relatedness. Treat it
			// as pending evidence instead of silently calling collection complete.
			jobs[result.index].LogCollectError = "GitLab 作业日志为空，无法分析失败 case"
			continue
		}
		jobs[result.index].LogExcerpt = result.excerpt
		jobs[result.index].LogTruncated = result.truncated
		jobs[result.index].LogCollected = true
		jobs[result.index].LogCollectError = ""
	}
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
func (c *Client) Retry(ctx context.Context, p model.TestPipeline) (model.PipelineObservation, error) {
	var out model.PipelineObservation
	if !validRequest(p) || p.PipelineID <= 0 || !model.CommitSHA.MatchString(p.ConfigSHA) {
		return out, &RejectedError{"测试重试不符合已登记 Pipeline、仓库和 SHA 约束"}
	}
	_, err := c.call(ctx, "POST", fmt.Sprintf("/pipelines/%d/retry", p.PipelineID), nil, &out)
	if err == nil {
		err = validate(out)
	}
	if err == nil && (out.ID != p.PipelineID || out.Ref != model.SeekDBTestRef || out.SHA != p.ConfigSHA) {
		err = fmt.Errorf("GitLab 重试响应与原 Pipeline 身份不匹配")
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
		collected, collectErr := c.failureJobs(ctx, out.ID, out.ProjectID, 0, map[int64]bool{})
		out.Jobs = collected.Jobs
		out.FailureSummary = collected.Summary
		if len(out.Jobs) > 0 {
			c.collectFailureLogs(ctx, out.Jobs)
			for _, job := range out.Jobs {
				if model.IsMySQLTestPipelineJob(job.Name) && (!job.LogCollected || job.LogCollectError != "" || strings.TrimSpace(job.LogExcerpt) == "") {
					out.FailureSummary.MySQLTestDetailsComplete = false
					break
				}
			}
		}
		if collectErr != nil {
			out.Jobs = append(out.Jobs, model.PipelineJob{Name: "作业明细暂不可用，请查看 pipeline 页面", Status: "unknown", FailureReason: collectErr.Error(), URL: out.URL, LogCollected: true, LogCollectError: collectErr.Error()})
		} else if len(out.Jobs) == 0 {
			out.FailureSummary.CollectionComplete = false
			out.FailureSummary.CollectionError = "pipeline 已失败，但 GitLab 未返回失败作业"
			out.Jobs = append(out.Jobs, model.PipelineJob{Name: "GitLab 未返回失败作业明细", Status: "unknown", URL: out.URL, LogCollected: true, LogCollectError: "pipeline 已失败，但 GitLab 未返回失败作业"})
		}
	}
	return out.PipelineObservation, nil
}

type failureJobCollection struct {
	Jobs            []model.PipelineJob
	Summary         model.PipelineFailureSummary
	mysqlDetails    int
	nonMySQLDetails int
}

func newFailureJobCollection() failureJobCollection {
	return failureJobCollection{Summary: model.PipelineFailureSummary{CollectionComplete: true, MySQLTestDetailsKnown: true, MySQLTestDetailsComplete: true}}
}

func (c *failureJobCollection) add(job model.PipelineJob) {
	if job.ID <= 0 || (job.Status != "failed" && job.Status != "canceled") {
		c.Summary.CollectionComplete = false
	} else {
		c.Summary.TotalFailures++
		if model.IsMySQLTestPipelineJob(job.Name) {
			c.Summary.MySQLTestFailures++
		} else {
			c.Summary.NonMySQLTestFailures++
		}
	}
	c.addDetail(job)
}

func (c *failureJobCollection) addDetail(job model.PipelineJob) {
	if model.IsMySQLTestPipelineJob(job.Name) {
		if c.mysqlDetails >= maxRecordedMySQLTestJobs {
			c.Summary.DetailTruncated = true
			c.Summary.MySQLTestDetailsComplete = false
			return
		}
		c.mysqlDetails++
		c.Jobs = append(c.Jobs, job)
		return
	}
	if c.nonMySQLDetails >= maxRecordedOtherFailedJobs {
		c.Summary.DetailTruncated = true
		return
	}
	c.nonMySQLDetails++
	c.Jobs = append(c.Jobs, job)
}

func (c *failureJobCollection) merge(other failureJobCollection) {
	c.Summary.TotalFailures += other.Summary.TotalFailures
	c.Summary.MySQLTestFailures += other.Summary.MySQLTestFailures
	c.Summary.NonMySQLTestFailures += other.Summary.NonMySQLTestFailures
	c.Summary.CollectionComplete = c.Summary.CollectionComplete && other.Summary.CollectionComplete
	c.Summary.DetailTruncated = c.Summary.DetailTruncated || other.Summary.DetailTruncated
	c.Summary.MySQLTestDetailsKnown = c.Summary.MySQLTestDetailsKnown && other.Summary.MySQLTestDetailsKnown
	c.Summary.MySQLTestDetailsComplete = c.Summary.MySQLTestDetailsComplete && other.Summary.MySQLTestDetailsComplete
	if c.Summary.CollectionError == "" {
		c.Summary.CollectionError = other.Summary.CollectionError
	}
	for _, job := range other.Jobs {
		c.addDetail(job)
	}
}

func (c *failureJobCollection) incomplete(err error) {
	c.Summary.CollectionComplete = false
	// A collection error can hide an additional mysqltest failure, so an Agent
	// must not certify a per-job mysqltest analysis from the partial list.
	c.Summary.MySQLTestDetailsComplete = false
	if err != nil && c.Summary.CollectionError == "" {
		c.Summary.CollectionError = short(err.Error(), 200)
	}
}

func (c *Client) failureJobs(ctx context.Context, pipelineID, projectID int64, depth int, seen map[int64]bool) (failureJobCollection, error) {
	result := newFailureJobCollection()
	if seen[pipelineID] || depth > 2 || len(seen) >= 10 {
		err := fmt.Errorf("子流水线超过安全采集范围，请查看 GitLab 页面")
		result.incomplete(err)
		return result, err
	}
	seen[pipelineID] = true
	for page := 1; page <= 5; page++ {
		var batch []model.PipelineJob
		h, err := c.call(ctx, "GET", fmt.Sprintf("/pipelines/%d/jobs?scope[]=failed&scope[]=canceled&per_page=100&page=%d", pipelineID, page), nil, &batch)
		if err != nil {
			result.incomplete(err)
			return result, err
		}
		for _, j := range batch {
			j.URL = fmt.Sprintf("%s/%s/-/jobs/%d", model.SeekDBTestHost, model.SeekDBTestProject, j.ID)
			j.Name = short(j.Name, 200)
			j.FailureReason = short(j.FailureReason, 200)
			result.add(j)
		}
		if h.Get("X-Next-Page") == "" && len(batch) < 100 {
			break
		}
		if page == 5 {
			err := fmt.Errorf("失败作业列表超过分页上限")
			result.incomplete(err)
			return result, err
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
		result.incomplete(err)
		return result, err
	}
	if h.Get("X-Next-Page") != "" || len(bridges) >= 100 {
		err := fmt.Errorf("子流水线列表超过采集上限")
		result.incomplete(err)
		return result, err
	}
	for _, b := range bridges {
		if b.Downstream == nil {
			continue
		}
		if projectID <= 0 || b.Downstream.ProjectID != projectID {
			err := fmt.Errorf("跨项目子流水线未授权自动采集")
			result.incomplete(err)
			return result, err
		}
		extra, err := c.failureJobs(ctx, b.Downstream.ID, projectID, depth+1, seen)
		result.merge(extra)
		if err != nil {
			result.incomplete(err)
			return result, err
		}
	}
	return result, nil
}
func short(value string, max int) string {
	r := []rune(value)
	if len(r) > max {
		return string(r[:max])
	}
	return value
}
