package runtimehost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
)

type developmentDirectoryAdapter struct {
	directory chan string
}

func (a *developmentDirectoryAdapter) Name() string { return "codex-agent" }

func (a *developmentDirectoryAdapter) Capabilities() map[string]any { return nil }

func (a *developmentDirectoryAdapter) Run(_ context.Context, _ model.RunSpec, directory string, _ <-chan model.Directive, _ func(agent.Event)) agent.Result {
	a.directory <- directory
	return agent.Result{ExitCode: 0, Output: `{}`}
}

func TestApprovedDevelopmentRunsInsideIsolatedRepository(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "sessions", "session_one")
	repository := filepath.Join(session, "repository")
	metadata := filepath.Join(root, "development", "session_one")
	for _, directory := range []string{repository, filepath.Join(metadata, "git")} {
		if err = os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	workspace := developmentWorkspace{
		Directory:  repository,
		GitDir:     filepath.Join(metadata, "git"),
		Repository: "oceanbase/seekdb",
		BaseBranch: "master",
	}
	if err = durableWorkspaceJSON(filepath.Join(metadata, "workspace.json"), workspace); err != nil {
		t.Fatal(err)
	}

	spool, err := OpenSpool(filepath.Join(root, "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	spec := model.RunSpec{
		RunID: "run_one", TaskID: "task_one", SessionID: "session_one", AgentID: "agent_one", AdapterID: "codex-agent",
		ExecutionGrant: &model.ExecutionGrant{ReviewID: "review_one", PlanHash: strings.Repeat("a", 64), Repository: "oceanbase/seekdb", BaseBranch: "master"},
	}
	raw, _ := json.Marshal(spec)
	if _, err = spool.AcceptRunStart(ctx, "message_one", raw, spec, session); err != nil {
		t.Fatal(err)
	}
	adapter := &developmentDirectoryAdapter{directory: make(chan string, 1)}
	daemon := &Daemon{config: Config{RuntimeID: "runtime_one", WorkRoot: root}, spool: spool, active: map[string]*activeRun{}}
	runCtx, runCancel := context.WithCancel(ctx)
	active := &activeRun{cancel: runCancel, directives: make(chan model.Directive, 1), spec: spec}
	daemon.active[spec.RunID] = active
	daemon.activeWG.Add(1)
	daemon.execute(runCtx, "message_one", spec, session, adapter, active)

	if got := <-adapter.directory; got != repository {
		t.Fatalf("development adapter directory = %q, want isolated repository %q", got, repository)
	}
}
