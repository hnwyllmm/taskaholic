package runtimehost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func TestDevelopmentPublisherReconcilesUncertainCreation(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	receipt := filepath.Join(directory, "receipt.json")
	spec := model.RunSpec{TaskID: "task_test", ExecutionGrant: &model.ExecutionGrant{PlanHash: strings.Repeat("a", 64)}}
	w := developmentWorkspace{Directory: directory, GitDir: filepath.Join(directory, "metadata"), Repository: "oceanbase/seekdb", BaseBranch: "master", Branch: "work-assistant/task_test", BaseSHA: strings.Repeat("a", 40)}
	posts, pushes := 0, 0
	remoteVisible := false
	remoteSHA := strings.Repeat("c", 40)
	w.command = func(_ context.Context, _ string, binary string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		if binary == "git" {
			switch {
			case strings.Contains(cmd, "rev-parse HEAD"):
				return strings.Repeat("b", 40), nil
			case strings.Contains(cmd, " push "):
				pushes++
				lease := "--force-with-lease=refs/heads/work-assistant/task_test:" + remoteSHA
				if pushes == 1 && strings.Contains(cmd, "--force") {
					t.Fatal("new branch was force-pushed")
				}
				if pushes == 2 && !strings.Contains(cmd, lease) {
					t.Fatal("existing task PR was not updated with the exact lease", cmd)
				}
				return "", nil
			default:
				return "", nil
			}
		}
		switch {
		case strings.Contains(cmd, "user --jq .login"):
			return "test-user", nil
		case strings.Contains(cmd, "repos/test-user/seekdb"):
			return `{"full_name":"test-user/seekdb","parent":{"full_name":"oceanbase/seekdb"}}`, nil
		case strings.Contains(cmd, "/pulls"):
			if !remoteVisible {
				return `[]`, nil
			}
			return `[{"html_url":"https://github.com/oceanbase/seekdb/pull/42","body":"<!-- work-assistant:development:task_test -->","state":"open","head":{"sha":"` + remoteSHA + `","ref":"work-assistant/task_test","repo":{"full_name":"test-user/seekdb"}},"base":{"ref":"master"}}]`, nil
		case strings.HasPrefix(cmd, "pr create"):
			posts++
			raw, err := os.ReadFile(receipt)
			if err != nil {
				t.Fatal("POST before durable intent", err)
			}
			var state developmentWorkspace
			_ = json.Unmarshal(raw, &state)
			if !state.CreateAttempted {
				t.Fatal("missing intent")
			}
			return "", errors.New("response lost")
		}
		t.Fatalf("unexpected command %s %s", binary, cmd)
		return "", nil
	}
	makeResult := func() workflow.Result {
		return workflow.Result{Outcome: "review", Message: "implemented", Artifacts: []workflow.File{}, PublishRequest: &workflow.PublishRequest{Title: "Implement", Body: "Tested boundaries"}}
	}
	first := makeResult()
	if err := publishDevelopment(ctx, &w, receipt, spec, &first); err == nil || posts != 1 || pushes != 1 {
		t.Fatal("expected uncertain create", err, posts, pushes)
	}
	second := makeResult()
	if err := publishDevelopment(ctx, &w, receipt, spec, &second); err == nil || posts != 1 || pushes != 1 {
		t.Fatal("blind retry", err, posts, pushes)
	}
	remoteVisible = true
	third := makeResult()
	if err := publishDevelopment(ctx, &w, receipt, spec, &third); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || pushes != 2 || len(third.PullRequests) != 1 || third.PublishRequest != nil || third.PullRequests[0].URL != "https://github.com/oceanbase/seekdb/pull/42" {
		t.Fatal("reconciliation lost PR")
	}
}

func TestDevelopmentPublisherRejectsUnverifiedExistingPRHead(t *testing.T) {
	w := developmentWorkspace{Directory: t.TempDir(), GitDir: t.TempDir(), Repository: "oceanbase/seekdb", BaseBranch: "master", Branch: "work-assistant/task_test", BaseSHA: strings.Repeat("a", 40)}
	pushes := 0
	w.command = func(_ context.Context, _ string, binary string, args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		if binary == "git" {
			if strings.Contains(cmd, "rev-parse HEAD") {
				return strings.Repeat("b", 40), nil
			}
			if strings.Contains(cmd, " push ") {
				pushes++
			}
			return "", nil
		}
		switch {
		case strings.Contains(cmd, "user --jq .login"):
			return "test-user", nil
		case strings.Contains(cmd, "repos/test-user/seekdb"):
			return `{"full_name":"test-user/seekdb","parent":{"full_name":"oceanbase/seekdb"}}`, nil
		case strings.Contains(cmd, "/pulls"):
			return `[{"html_url":"https://github.com/oceanbase/seekdb/pull/42","body":"<!-- work-assistant:development:task_test -->","state":"open","head":{"sha":"` + strings.Repeat("c", 40) + `","ref":"work-assistant/task_test","repo":{"full_name":"someone-else/seekdb"}},"base":{"ref":"master"}}]`, nil
		}
		t.Fatalf("unexpected command %s %s", binary, cmd)
		return "", nil
	}
	spec := model.RunSpec{TaskID: "task_test", ExecutionGrant: &model.ExecutionGrant{PlanHash: strings.Repeat("a", 64)}}
	result := workflow.Result{PublishRequest: &workflow.PublishRequest{Title: "Implement", Body: "Tested"}}
	err := publishDevelopment(context.Background(), &w, "", spec, &result)
	if err == nil || !strings.Contains(err.Error(), "does not match the verified publisher branch") || pushes != 0 {
		t.Fatal("unverified PR head reached push", err, pushes)
	}
}

func TestDevelopmentPublisherRejectsForeignFork(t *testing.T) {
	w := developmentWorkspace{Directory: t.TempDir(), Repository: "oceanbase/seekdb"}
	calls := 0
	w.command = func(_ context.Context, _ string, b string, args ...string) (string, error) {
		calls++
		if calls == 1 {
			return "actor", nil
		}
		if calls == 2 {
			return `{"full_name":"actor/seekdb","parent":{"full_name":"other/seekdb"}}`, nil
		}
		t.Fatal("wrote before fork identity verified")
		return "", nil
	}
	err := publishDevelopment(context.Background(), &w, "", model.RunSpec{}, &workflow.Result{PublishRequest: &workflow.PublishRequest{Title: "change", Body: "body"}})
	if err == nil || calls != 2 {
		t.Fatal("foreign fork accepted", err)
	}
}

func TestDevelopmentWorkspaceRejectsChangedScopeAndSymlink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "sessions", "session_test")
	metadata := filepath.Join(root, "development", "session_test")
	for _, p := range []string{session, metadata} {
		if err = os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	w := developmentWorkspace{Directory: filepath.Join(session, "repository"), GitDir: filepath.Join(metadata, "git"), Repository: "oceanbase/seekdb", BaseBranch: "master"}
	if err = durableWorkspaceJSON(filepath.Join(metadata, "workspace.json"), w); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(t.TempDir(), w.Directory); err != nil {
		t.Fatal(err)
	}
	d := Daemon{config: Config{WorkRoot: root}}
	spec := model.RunSpec{TaskID: "task_test", SessionID: "session_test", AdapterID: "codex-agent", ExecutionGrant: &model.ExecutionGrant{ReviewID: "review_test", PlanHash: strings.Repeat("a", 64), Repository: w.Repository, BaseBranch: w.BaseBranch}}
	if _, _, e := d.prepareDevelopment(context.Background(), spec, session); e == nil {
		t.Fatal("symlinked checkout accepted")
	}
	spec.ExecutionGrant.Repository = "other/repo"
	if _, _, e := d.prepareDevelopment(context.Background(), spec, session); e == nil {
		t.Fatal("scope changed without new workspace")
	}
	spec.ReadOnly = true
	if _, _, e := d.prepareDevelopment(context.Background(), spec, session); e == nil {
		t.Fatal("read-only run got write grant")
	}
}
