package gitlabci

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func requestFixture() model.TestPipeline {
	return model.TestPipeline{ID: "request-1", TaskID: "task-1", Kind: model.SeekDBTestKind, PRURL: "https://github.com/oceanbase/seekdb/pull/123", HeadSHA: strings.Repeat("a", 40), PipelineID: 41, ConfigSHA: strings.Repeat("b", 40), SubmittedAtMS: time.Now().UnixMilli()}
}
func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Client{HTTP: server.Client(), base: server.URL, Token: func() (string, error) { return "private-test-token", nil }}
}

func TestCreatePinsCodeSHAAndFindReconcilesMarkerAndReadsChildFailures(t *testing.T) {
	p := requestFixture()
	posts, retries := 0, 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "private-test-token" {
			t.Error("missing auth")
		}
		prefix := "/api/v4/projects/obqa%2Fseekdb_test"
		if !strings.HasPrefix(r.URL.EscapedPath(), prefix) {
			t.Error("escaped project not pinned", r.URL.EscapedPath())
		}
		path := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		pipeline := model.PipelineObservation{ID: 41, SHA: p.ConfigSHA, Ref: model.SeekDBTestRef, Status: "running", URL: "https://untrusted.example/pipeline"}
		switch path {
		case "/pipeline":
			if r.Method != "POST" {
				t.Error(r.Method)
			}
			posts++
			var body struct {
				Ref       string     `json:"ref"`
				Variables []variable `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Ref != model.SeekDBTestRef || !matches(body.Variables, p) || len(body.Variables) != 4 {
				t.Error("unscoped create", body)
			}
			write(pipeline)
		case "/pipelines/41/retry":
			if r.Method != "POST" {
				t.Error(r.Method)
			}
			retries++
			write(pipeline)
		case "/pipelines":
			if r.URL.Query().Get("source") != "api" || r.URL.Query().Get("ref") != model.SeekDBTestRef || r.URL.Query().Get("created_after") == "" {
				t.Error("unbounded lookup")
			}
			write([]model.PipelineObservation{pipeline})
		case "/pipelines/41/variables":
			write(append(variables(p), variable{"UNRELATED_SECRET", "must-never-be-returned"}))
		case "/pipelines/41":
			write(map[string]any{"id": 41, "sha": p.ConfigSHA, "ref": model.SeekDBTestRef, "status": "failed", "project_id": 2537})
		case "/pipelines/41/jobs":
			write([]any{})
		case "/pipelines/41/bridges":
			write([]any{map[string]any{"downstream_pipeline": map[string]any{"id": 42, "project_id": 2537}}})
		case "/pipelines/42/jobs":
			write([]model.PipelineJob{{ID: 9, Name: "persistence-recovery", Status: "failed", FailureReason: "script_failure", URL: "javascript:bad()"}})
		case "/pipelines/42/bridges":
			write([]any{})
		case "/jobs/9/trace":
			_, _ = w.Write([]byte("setup\nPRIVATE-TOKEN: must-never-be-returned\n\x1b[31massertion failed at recovery_test.cpp:91\x1b[0m\n"))
		default:
			t.Error("unexpected request", path)
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	created, err := client.Create(ctx, p)
	if err != nil || posts != 1 || created.URL != model.PipelineURL(41) {
		t.Fatal(created, err, posts)
	}
	retried, err := client.Retry(ctx, p)
	if err != nil || retries != 1 || retried.ID != p.PipelineID || retried.SHA != p.ConfigSHA {
		t.Fatal(retried, err, retries)
	}
	found, err := client.Find(ctx, p)
	if err != nil || found == nil || found.ID != 41 || posts != 1 {
		t.Fatal(found, err)
	}
	observed, err := client.Observe(ctx, p)
	if err != nil || observed.Status != "failed" || len(observed.Jobs) != 1 || observed.Jobs[0].ID != 9 || !strings.HasPrefix(observed.Jobs[0].URL, model.SeekDBTestHost) || !observed.Jobs[0].LogCollected || !strings.Contains(observed.Jobs[0].LogExcerpt, "assertion failed") {
		t.Fatal(observed, err)
	}
	raw, _ := json.Marshal(observed)
	if strings.Contains(string(raw), "must-never") || strings.Contains(string(raw), "javascript:") || strings.Contains(string(raw), "\x1b") {
		t.Fatal("external content/secret escaped boundary")
	}
}

func TestJobTraceFailureIsBoundedAndNeverLeaksResponseBody(t *testing.T) {
	p := requestFixture()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/projects/obqa%2Fseekdb_test")
		switch path {
		case "/pipelines/41":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 41, "sha": p.ConfigSHA, "ref": model.SeekDBTestRef, "status": "failed", "project_id": 2537})
		case "/pipelines/41/variables":
			_ = json.NewEncoder(w).Encode(variables(p))
		case "/pipelines/41/jobs":
			_ = json.NewEncoder(w).Encode([]model.PipelineJob{{ID: 77, Name: "failed", Status: "failed"}})
		case "/pipelines/41/bridges":
			_ = json.NewEncoder(w).Encode([]any{})
		case "/jobs/77/trace":
			http.Error(w, "SECRET-BODY-MUST-NOT-LEAK", http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	})
	observed, err := client.Observe(context.Background(), p)
	if err != nil || len(observed.Jobs) != 1 || observed.Jobs[0].LogCollected || !strings.Contains(observed.Jobs[0].LogCollectError, "401") || strings.Contains(observed.Jobs[0].LogCollectError, "SECRET-BODY") {
		t.Fatal(observed, err)
	}
}

func TestObserveCountsAllFailedJobsWhileBoundingStoredDetails(t *testing.T) {
	p := requestFixture()
	var traces atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/projects/obqa%2Fseekdb_test")
		switch path {
		case "/pipelines/41":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 41, "sha": p.ConfigSHA, "ref": model.SeekDBTestRef, "status": "failed", "project_id": 2537})
		case "/pipelines/41/variables":
			_ = json.NewEncoder(w).Encode(variables(p))
		case "/pipelines/41/jobs":
			jobs := make([]model.PipelineJob, 0, 31)
			for i := 0; i < 31; i++ {
				name := fmt.Sprintf("suite-%02d", i)
				if i < 4 {
					name = fmt.Sprintf("mysqltest-%02d", i)
				}
				jobs = append(jobs, model.PipelineJob{ID: int64(100 + i), Name: name, Status: "failed", FailureReason: "script_failure"})
			}
			_ = json.NewEncoder(w).Encode(jobs)
		case "/pipelines/41/bridges":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			if strings.HasPrefix(path, "/jobs/") && strings.HasSuffix(path, "/trace") {
				traces.Add(1)
				_, _ = w.Write([]byte("bounded failure trace"))
				return
			}
			http.NotFound(w, r)
		}
	})
	observed, err := client.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Jobs) != 4+maxRecordedOtherFailedJobs || observed.FailureSummary.TotalFailures != 31 || observed.FailureSummary.MySQLTestFailures != 4 || observed.FailureSummary.NonMySQLTestFailures != 27 || !observed.FailureSummary.CollectionComplete || !observed.FailureSummary.DetailTruncated || !observed.FailureSummary.MySQLTestDetailsKnown || !observed.FailureSummary.MySQLTestDetailsComplete || int(traces.Load()) != 4+maxTracedOtherFailedJobs {
		t.Fatal(observed)
	}
}

func TestObserveReadsEveryMySQLTestTraceBeforeAgentAnalysis(t *testing.T) {
	p := requestFixture()
	var traces atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/projects/obqa%2Fseekdb_test")
		switch path {
		case "/pipelines/41":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 41, "sha": p.ConfigSHA, "ref": model.SeekDBTestRef, "status": "failed", "project_id": 2537})
		case "/pipelines/41/variables":
			_ = json.NewEncoder(w).Encode(variables(p))
		case "/pipelines/41/jobs":
			jobs := make([]model.PipelineJob, 0, 36)
			for i := 0; i < 36; i++ {
				name := fmt.Sprintf("mysqltest-%02d", i)
				if i >= 33 {
					name = fmt.Sprintf("other-suite-%02d", i)
				}
				jobs = append(jobs, model.PipelineJob{ID: int64(100 + i), Name: name, Status: "failed", FailureReason: "script_failure"})
			}
			_ = json.NewEncoder(w).Encode(jobs)
		case "/pipelines/41/bridges":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			if strings.HasPrefix(path, "/jobs/") && strings.HasSuffix(path, "/trace") {
				traces.Add(1)
				_, _ = w.Write([]byte("mysqltest result: 1 failure"))
				return
			}
			http.NotFound(w, r)
		}
	})
	observed, err := client.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Jobs) != 36 || observed.FailureSummary.MySQLTestFailures != 33 || observed.FailureSummary.NonMySQLTestFailures != 3 || !observed.FailureSummary.MySQLTestDetailsKnown || !observed.FailureSummary.MySQLTestDetailsComplete || observed.FailureSummary.DetailTruncated || traces.Load() != 36 {
		t.Fatal(observed)
	}
	for _, job := range observed.Jobs {
		if model.IsMySQLTestPipelineJob(job.Name) && (!job.LogCollected || job.LogCollectError != "" || !strings.Contains(job.LogExcerpt, "1 failure")) {
			t.Fatal("mysqltest trace was not retained", job)
		}
	}
}
func TestGitLabIdentityMismatchAndErrorsNeverBecomeSuccessOrLeakSecrets(t *testing.T) {
	p := requestFixture()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/variables") {
			_ = json.NewEncoder(w).Encode(variables(model.TestPipeline{ID: p.ID, HeadSHA: strings.Repeat("c", 40)}))
			return
		}
		_ = json.NewEncoder(w).Encode(model.PipelineObservation{ID: 41, Ref: model.SeekDBTestRef, SHA: p.ConfigSHA, Status: "success"})
	})
	if _, err := client.Observe(context.Background(), p); err == nil {
		t.Fatal("wrong tested SHA counted as success")
	}
	for _, status := range []int{400, 401, 403, 404, 422, 429, 500, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("SECRET-VALUE-MUST-NOT-LEAK"))
			})
			_, err := client.Create(context.Background(), p)
			if err == nil || strings.Contains(err.Error(), "SECRET-VALUE") {
				t.Fatal(err)
			}
			if DefinitelyRejected(err) != (status < 429) {
				t.Fatal("unsafe retry classification", status, err)
			}
		})
	}
}
func TestGitLabRedirectDoesNotForwardCredentialOrRepeatPost(t *testing.T) {
	calls := 0
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer outside.Close()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, outside.URL, http.StatusTemporaryRedirect)
	})
	if _, err := c.Create(context.Background(), requestFixture()); err == nil {
		t.Fatal("redirect followed")
	}
	if calls != 0 {
		t.Fatal("credential sent to redirected host")
	}
}
