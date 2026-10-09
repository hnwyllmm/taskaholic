package tasksource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func TestAntMulticaExactFilterPaginationAndNoReplay(t *testing.T) {
	properties := `[{"id":"iteration","name":"迭代","config":{"options":[{"id":"v15","name":"1.5.0"},{"id":"v16","name":"1.6.0"}]}}]`
	matching := multicaIssue{ID: "one", Identifier: "SEEK-1", WorkspaceID: "workspace", AssigneeID: "me", AssigneeType: "member", Title: "task", Description: "scope", Status: "todo", StatusCategory: "todo", Properties: map[string]json.RawMessage{"iteration": json.RawMessage(`"v15"`)}}
	wrongPerson := matching
	wrongPerson.ID = "other"
	wrongPerson.AssigneeID = "somebody-else"
	wrongVersion := matching
	wrongVersion.ID = "v16"
	wrongVersion.Properties = map[string]json.RawMessage{"iteration": json.RawMessage(`"v16"`)}
	closed := matching
	closed.ID = "done"
	closed.StatusCategory = "done"
	pages := 0
	provider := AntMultica{Binary: "multica", Run: func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		if binary != "multica" || !slices.Contains(args, "https://antmultica.alipay.com") || !slices.Contains(args, "workspace") {
			t.Fatal("unscoped command", args)
		}
		if slices.Contains(args, "property") {
			return []byte(properties), nil
		}
		if !slices.Contains(args, "--assignee-id") || !slices.Contains(args, "me") {
			t.Fatal("missing exact assignment filter")
		}
		pages++
		offset := args[slices.Index(args, "--offset")+1]
		if offset == "0" {
			raw, _ := json.Marshal(map[string]any{"issues": []multicaIssue{wrongPerson, wrongVersion}, "has_more": true})
			return raw, nil
		}
		if offset != "2" {
			t.Fatal("wrong pagination", args)
		}
		raw, _ := json.Marshal(map[string]any{"issues": []multicaIssue{matching, closed}, "has_more": false})
		return raw, nil
	}}
	source := model.TaskSource{Config: model.SourceConfig{WorkspaceID: "workspace", WorkspaceSlug: "seekdb", AssigneeID: "me", IterationKey: "迭代", IterationValue: "1.5.0"}}
	target := model.SourceTarget{Cursor: json.RawMessage("{}")}
	first, err := provider.Poll(context.Background(), source, target)
	if err != nil || len(first.Events) != 1 || pages != 2 {
		t.Fatal(first, err, pages)
	}
	if first.Events[0].Title != "SEEK-1 task" {
		t.Fatal(first.Events)
	}
	if first.Events[0].URL != "https://antmultica.alipay.com/seekdb/issues/one" || !strings.Contains(first.Events[0].Message, first.Events[0].URL) {
		t.Fatal("missing direct issue link", first.Events)
	}
	target.Cursor = first.Cursor
	second, err := provider.Poll(context.Background(), source, target)
	if err != nil || len(second.Events) != 0 {
		t.Fatal("replayed unchanged work", second, err)
	}
	matching.Status = "in_progress"
	matching.StatusCategory = "in_progress"
	statusOnly, err := provider.Poll(context.Background(), source, target)
	if err != nil || len(statusOnly.Events) != 0 || string(statusOnly.Cursor) == string(target.Cursor) {
		t.Fatal("status-only lifecycle writeback became new work", statusOnly, err)
	}
	target.Cursor = statusOnly.Cursor
	matching.Description = "updated scope"
	changed, err := provider.Poll(context.Background(), source, target)
	if err != nil || len(changed.Events) != 1 || !strings.Contains(changed.Events[0].Message, "updated scope") {
		t.Fatal("business content update was suppressed", changed, err)
	}
	source.Config.IterationValue = "missing"
	if _, err = provider.Poll(context.Background(), source, target); err == nil {
		t.Fatal("unknown iteration broadened scope")
	}
}

func TestAntMulticaDoesNotCommitPartialPage(t *testing.T) {
	call := 0
	provider := AntMultica{Run: func(context.Context, string, ...string) ([]byte, error) {
		call++
		switch call {
		case 1:
			return []byte(`[{"id":"iteration","name":"迭代","config":{"options":[{"id":"v15","name":"1.5.0"}]}}]`), nil
		case 2:
			return []byte(`{"issues":[{"id":"one","workspace_id":"workspace","assignee_id":"me","assignee_type":"member","title":"task","properties":{"iteration":"v15"}}],"has_more":true}`), nil
		default:
			return nil, errors.New("transport failed")
		}
	}}
	source := model.TaskSource{Config: model.SourceConfig{WorkspaceID: "workspace", WorkspaceSlug: "seekdb", AssigneeID: "me", IterationKey: "迭代", IterationValue: "1.5.0"}}
	result, err := provider.Poll(context.Background(), source, model.SourceTarget{Cursor: json.RawMessage("{}")})
	if err == nil || result.Cursor != nil {
		t.Fatal("partial pagination advanced cursor")
	}
}

func response(code int, headers string, value any) []byte {
	body, _ := json.Marshal(value)
	if code == 304 {
		body = nil
	}
	return []byte(fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n%s\r\n%s", code, httpStatus(code), len(body), headers, body))
}
func httpStatus(code int) string {
	switch code {
	case 200:
		return "OK"
	case 304:
		return "Not Modified"
	case 429:
		return "Too Many Requests"
	default:
		return "Error"
	}
}

func TestGitHubConditionalPollingCommentsCIAndNewHead(t *testing.T) {
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	since := time.Now().Add(-time.Minute)
	stamp := time.Now().UTC().Format(time.RFC3339)
	comment := func(id int, body string) map[string]any {
		return map[string]any{"id": id, "body": body, "updated_at": stamp, "user": map[string]string{"login": "human"}, "html_url": "https://github.com/o/r/pull/1#issuecomment-1"}
	}
	etags := map[string]string{}
	conditional := 0
	failSecondPage := false
	provider := GitHub{Binary: "gh", Run: func(_ context.Context, binary string, args ...string) ([]byte, error) {
		if binary != "gh" || !slices.Contains(args, "GET") || !slices.Contains(args, "github.com") {
			t.Fatal("unsafe CLI command", args)
		}
		path := args[len(args)-1]
		var value any
		link := ""
		switch {
		case path == "/repos/o/r/pulls/1":
			value = map[string]any{"number": 1, "state": "open", "head": map[string]string{"sha": head}, "user": map[string]string{"login": "human"}, "base": map[string]any{"repo": map[string]any{"id": 123, "full_name": "o/r"}}}
		case strings.Contains(path, "/files"):
			t.Fatal("source must not download review code", path)
		case strings.Contains(path, "/issues/1/comments"):
			if strings.Contains(path, "page=2") {
				if failSecondPage {
					return nil, errors.New("network error")
				}
				value = []any{comment(2, "second page actionable feedback")}
			} else {
				old := comment(3, "before registration")
				old["updated_at"] = since.Add(-time.Hour).UTC().Format(time.RFC3339)
				value = []any{comment(1, "please improve"), comment(4, "<!-- work-assistant:task-reply --> done"), old}
				link = "Link: <https://api.github.com/repositories/123/issues/1/comments?per_page=100&page=2>; rel=\"next\"\r\n"
			}
		case strings.Contains(path, "/pulls/1/comments"):
			old := comment(5, "outdated")
			old["commit_id"] = strings.Repeat("c", 40)
			value = []any{old}
		case strings.Contains(path, "/reviews"):
			review := comment(6, "")
			review["state"] = "CHANGES_REQUESTED"
			review["commit_id"] = head
			value = []any{review}
		case strings.Contains(path, "check-runs"):
			value = map[string]any{"check_runs": []any{map[string]any{"id": 42, "name": "tests", "head_sha": head, "status": "completed", "conclusion": "failure", "completed_at": stamp}}}
		case strings.Contains(path, "/statuses"):
			value = []any{map[string]any{"id": 7, "context": "build", "state": "success"}, map[string]any{"id": 6, "context": "build", "state": "failure"}, map[string]any{"id": 5, "context": "legacy-ci", "state": "failure"}}
		default:
			t.Fatal("unexpected endpoint", path)
		}
		etag := digest(value)
		etagHeader := "\"" + etag + "\""
		etags[path] = etagHeader
		if slices.Contains(args, "If-None-Match: "+etagHeader) {
			conditional++
			return response(304, "", nil), errors.New("not modified")
		}
		return response(200, "ETag: "+etagHeader+"\r\n"+link, value), nil
	}}
	source := model.TaskSource{Kind: "github"}
	target := model.SourceTarget{Entity: "https://github.com/o/r/pull/1", CreatedAtMS: since.UnixMilli(), Cursor: json.RawMessage("{}")}
	first, err := provider.Poll(ctx, source, target)
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, e := range first.Events {
		count[e.Kind]++
	}
	if count["github.head"] != 1 || count["github.comment"] != 3 || count["github.ci_failed"] != 2 {
		t.Fatal("unexpected event filters", count, first.Events)
	}
	target.Cursor = first.Cursor
	second, err := provider.Poll(ctx, source, target)
	if err != nil || len(second.Events) != 0 || conditional < 5 {
		t.Fatal("conditional/idempotent poll failed", second, conditional, err)
	}
	head = strings.Repeat("b", 40)
	third, err := provider.Poll(ctx, source, target)
	if err != nil {
		t.Fatal(err)
	}
	newHeads := 0
	for _, e := range third.Events {
		if e.Kind == "github.head" && e.HeadSHA == head {
			newHeads++
		}
	}
	if newHeads != 1 {
		t.Fatal("missing new revision", third.Events)
	}
	failSecondPage = true
	failed, err := provider.Poll(ctx, source, target)
	if err == nil || failed.Cursor != nil {
		t.Fatal("partial GitHub snapshot committed")
	}
}

func TestGitHubSuccessfulCITerminalSnapshotWakesRootOnceAfterStablePoll(t *testing.T) {
	head := strings.Repeat("d", 40)
	checkStatus, conclusion := "queued", ""
	provider := GitHub{Binary: "gh", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		path := args[len(args)-1]
		var value any
		switch {
		case path == "/repos/o/r/pulls/9":
			value = map[string]any{
				"number": 9, "state": "open", "title": "stable CI",
				"head": map[string]string{"sha": head},
				"base": map[string]any{"repo": map[string]any{"id": 99, "full_name": "o/r"}},
			}
		case strings.Contains(path, "/issues/9/comments"), strings.Contains(path, "/pulls/9/comments"), strings.Contains(path, "/pulls/9/reviews"), strings.Contains(path, "/statuses"):
			value = []any{}
		case strings.Contains(path, "/check-runs"):
			value = map[string]any{"check_runs": []any{map[string]any{
				"id": 71, "name": "build", "head_sha": head, "status": checkStatus,
				"conclusion": conclusion, "completed_at": "2026-09-14T04:00:00Z",
			}}}
		default:
			t.Fatal("unexpected endpoint", path)
		}
		return response(200, "", value), nil
	}}
	target := model.SourceTarget{Entity: "https://github.com/o/r/pull/9", CreatedAtMS: time.Now().Add(-time.Hour).UnixMilli(), Cursor: json.RawMessage("{}")}
	poll := func() PollResult {
		result, err := provider.Poll(context.Background(), model.TaskSource{Kind: "github"}, target)
		if err != nil {
			t.Fatal(err)
		}
		target.Cursor = result.Cursor
		return result
	}
	if first := poll(); len(first.Events) != 1 || first.Events[0].Kind != "github.head" {
		t.Fatal("queued CI emitted a terminal event", first.Events)
	} else {
		var cursor githubCursor
		if err := json.Unmarshal(first.Cursor, &cursor); err != nil || cursor.CIState != "pending" {
			t.Fatal("root CI gate did not record pending state", cursor, err)
		}
	}
	checkStatus, conclusion = "completed", "success"
	if firstPassing := poll(); len(firstPassing.Events) != 0 {
		t.Fatal("one passing snapshot must not resume the root", firstPassing.Events)
	} else {
		var cursor githubCursor
		if err := json.Unmarshal(firstPassing.Cursor, &cursor); err != nil || cursor.CIState != "pending" {
			t.Fatal("one passing snapshot prematurely opened the root gate", cursor, err)
		}
	}
	stable := poll()
	if len(stable.Events) != 1 || stable.Events[0].Kind != "github.ci_succeeded" || stable.Events[0].HeadSHA != head || !strings.Contains(stable.Events[0].Message, "1 项 GitHub CI") {
		t.Fatal("stable passing snapshot did not emit one success", stable.Events)
	}
	var cursor githubCursor
	if err := json.Unmarshal(stable.Cursor, &cursor); err != nil || cursor.CIState != "success" {
		t.Fatal("stable passing snapshot did not open the root gate", cursor, err)
	}
	if repeated := poll(); len(repeated.Events) != 0 {
		t.Fatal("passing CI replayed", repeated.Events)
	}
}

func TestGitHubRateLimitAndPaginationDomainGuard(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, kind := range []string{"rate", "evil", "other_name", "other_id", "unverified_id", "mismatched_identity"} {
		calls := 0
		g := GitHub{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls++
			if kind == "rate" {
				return response(429, "Retry-After: 1800\r\n", map[string]string{"message": "slow down"}), errors.New("429")
			}
			if calls == 1 {
				pr := map[string]any{"number": 1, "state": "open", "head": map[string]string{"sha": head}}
				if kind != "unverified_id" {
					name := "o/r"
					if kind == "mismatched_identity" {
						name = "o/other"
					}
					pr["base"] = map[string]any{"repo": map[string]any{"id": 123, "full_name": name}}
				}
				return response(200, "", pr), nil
			}
			if calls == 2 {
				if kind == "mismatched_identity" {
					t.Fatal("continued polling a different base repository")
				}
				link := map[string]string{
					"evil":          "https://evil.test/repositories/123/pulls/1/files?page=2",
					"other_name":    "https://api.github.com/repos/o/other/pulls/1/files?page=2",
					"other_id":      "https://api.github.com/repositories/1234/pulls/1/files?page=2",
					"unverified_id": "https://api.github.com/repositories/123/pulls/1/files?page=2",
				}[kind]
				return response(200, "Link: <"+link+">; rel=\"next\"\r\n", []any{}), nil
			}
			t.Fatal("credentialed request sent to an untrusted next link")
			return nil, nil
		}}
		_, err := g.Poll(context.Background(), model.TaskSource{}, model.SourceTarget{Entity: "https://github.com/o/r/pull/1", Cursor: json.RawMessage("{}")})
		if err == nil {
			t.Fatal("missing failure", kind)
		}
		if kind == "rate" {
			var retry *RetryError
			if !errors.As(err, &retry) || !retry.Global || time.Until(retry.At) < 29*time.Minute {
				t.Fatal("Retry-After ignored", err)
			}
		}
	}
}

func TestProviderSubprocessDoesNotInheritControlSecrets(t *testing.T) {
	t.Setenv("WORK_ASSISTANT_RUNTIME_TOKEN", "test-secret-not-for-providers")
	t.Setenv("WORK_ASSISTANT_API_TOKEN", "test-secret-not-for-providers")
	raw, err := RunCLI(context.Background(), "/bin/sh", "-c", `test -z "$WORK_ASSISTANT_RUNTIME_TOKEN" && test -z "$WORK_ASSISTANT_API_TOKEN" && test "$GH_PROMPT_DISABLED" = 1 && printf ok`)
	if err != nil || string(raw) != "ok" {
		t.Fatal("provider inherited a control credential", err)
	}
}
