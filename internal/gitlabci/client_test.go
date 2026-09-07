package gitlabci

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	posts := 0
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
	found, err := client.Find(ctx, p)
	if err != nil || found == nil || found.ID != 41 || posts != 1 {
		t.Fatal(found, err)
	}
	observed, err := client.Observe(ctx, p)
	if err != nil || observed.Status != "failed" || len(observed.Jobs) != 1 || observed.Jobs[0].ID != 9 || !strings.HasPrefix(observed.Jobs[0].URL, model.SeekDBTestHost) {
		t.Fatal(observed, err)
	}
	raw, _ := json.Marshal(observed)
	if strings.Contains(string(raw), "must-never") || strings.Contains(string(raw), "javascript:") {
		t.Fatal("external content/secret escaped boundary")
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
