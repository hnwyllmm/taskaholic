package upgrade

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/agent"
	protection "work-assistant/internal/backup"
	"work-assistant/internal/model"
	"work-assistant/internal/runtimehost"
	"work-assistant/internal/store"
)

type editReader struct {
	inspect func(model.RunSpec, string)
	output  string
}

func (editReader) Name() string                 { return "cursor-agent" }
func (editReader) Capabilities() map[string]any { return nil }
func (r editReader) Run(_ context.Context, spec model.RunSpec, directory string, _ <-chan model.Directive, _ func(agent.Event)) agent.Result {
	if r.inspect != nil {
		r.inspect(spec, directory)
	}
	return agent.Result{Output: r.output}
}

func TestCursorBuilderUsesReadOnlyProposalThenTrustedCandidateEdits(t *testing.T) {
	root, data := testProject(t)
	config := testManagerConfig(root, data)
	config.RuntimeID, config.AdapterID = "local", "cursor-agent"
	reader := editReader{output: `{"summary":"update greeting","edits":[{"path":"internal/greeting/greeting.go","old_text":"old","new_text":"new","create":false}]}`, inspect: func(spec model.RunSpec, directory string) {
		if filepath.Base(directory) != "builder-workspace" || !strings.Contains(spec.Instructions, "candidate") || !strings.Contains(string(spec.OutputSchema), "old_text") || spec.ModelID != "pinned-model" {
			t.Fatalf("wrong builder context: %+v %s", spec, directory)
		}
	}}
	manager, err := New(config, cursorBuilder{reader: reader})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "cursor-proposal", Instructions: "change greeting", Builder: &model.AgentProfile{RuntimeID: "local", AdapterID: "cursor-agent", ModelID: "pinned-model"}})
	if u.State != "READY" {
		t.Fatalf("%s\n%s", u.Error, u.Log)
	}
	body, _ := os.ReadFile(filepath.Join(root, "internal/greeting/greeting.go"))
	if !strings.Contains(string(body), "old") {
		t.Fatal("live source was changed")
	}
	if _, err := os.Stat(filepath.Join(manager.candidateDir(u.ID), ".cursor")); !os.IsNotExist(err) {
		t.Fatal("CLI metadata leaked into candidate")
	}
}

func TestCursorEditsValidateAllPathsAndMatchesBeforeWriting(t *testing.T) {
	for _, bad := range []cursorEdit{
		{Path: "../escape", Create: true, New: "x"}, {Path: "/tmp/escape", Create: true, New: "x"},
		{Path: "internal/../README.md", Old: "old", New: "x"}, {Path: "internal\\escape", Create: true, New: "x"},
		{Path: "go.mod", Old: "module", New: "x"}, {Path: "internal/localconfig/new.go", Create: true, New: "x"},
		{Path: "internal/store/new.go", Create: true, New: "x"}, {Path: "data/private", Create: true, New: "x"},
		{Path: "README.md", Old: "missing", New: "x"}, {Path: "README.md", Create: true, New: "x"},
		{Path: "README.md", Old: "", New: "x"}, {Path: "README.md", Old: "readme", New: "\x00"},
	} {
		t.Run(bad.Path+bad.Old, func(t *testing.T) {
			root, _ := testProject(t)
			err := applyCursorEdits(root, cursorEdits{Summary: "test", Edits: []cursorEdit{{Path: "README.md", Old: "old", New: "new"}, bad}})
			if err == nil {
				t.Fatalf("unsafe edit accepted: %+v", bad)
			}
			body, _ := os.ReadFile(filepath.Join(root, "README.md"))
			if string(body) != "old readme\n" {
				t.Fatal("partially applied invalid proposal")
			}
		})
	}
	root, _ := testProject(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "internal/link")); err != nil {
		t.Fatal(err)
	}
	if err := applyCursorEdits(root, cursorEdits{Summary: "link", Edits: []cursorEdit{{Path: "internal/link/new.go", Create: true, New: "x"}}}); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if err := os.Link(filepath.Join(root, "README.md"), filepath.Join(root, "internal/linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := applyCursorEdits(root, cursorEdits{Summary: "link", Edits: []cursorEdit{{Path: "internal/linked.md", Old: "old", New: "new"}}}); err == nil {
		t.Fatal("hard link accepted")
	}
}

func TestCursorEditsAllowSequentialChangesAndNewFiles(t *testing.T) {
	root, _ := testProject(t)
	err := applyCursorEdits(root, cursorEdits{Summary: "safe", Edits: []cursorEdit{
		{Path: "README.md", Old: "old", New: "new"}, {Path: "README.md", Old: "new readme", New: "updated readme"},
		{Path: "internal/greeting/new_test.go", Create: true, New: "package greeting\n"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(root, "README.md"))
	if string(body) != "updated readme\n" {
		t.Fatal(string(body))
	}
}

func TestUpgradeBackupIncludesRuntimeAndNeverRestoresDatabases(t *testing.T) {
	root, data := testProject(t)
	m, err := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		return os.WriteFile(filepath.Join(directory, "README.md"), []byte("new\n"), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u := m.Prepare(ctx, model.Upgrade{ID: "both-databases", Instructions: "docs"})
	if u.State != "READY" {
		t.Fatal(u.Error)
	}
	state, err := store.Open(filepath.Join(data, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	spool, err := runtimehost.OpenSpool(filepath.Join(data, "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	backup, err := m.Backup(ctx, state, u)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"control.sqlite", "runtime.sqlite"} {
		if err := protection.VerifySQLite(ctx, filepath.Join(backup, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Apply(u); err != nil {
		t.Fatal(err)
	}
	// A record written after the backup must survive source/binary rollback.
	record, err := state.CreateUpgrade(ctx, "after-backup", "preserve this record")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rollback(u); err != nil {
		t.Fatal(err)
	}
	if _, err := state.GetUpgrade(ctx, record.ID); err != nil {
		t.Fatal("rollback lost data", err)
	}
}

// Optional real CLI smoke test. It operates exclusively on a disposable tiny
// project, never on the deployed source/data or any user's upgrade request.
func TestRealCursorCandidate(t *testing.T) {
	binary := os.Getenv("WORK_ASSISTANT_TEST_CURSOR")
	if binary == "" || os.Getenv("WORK_ASSISTANT_VALIDATION_SANDBOX") != "" {
		t.Skip("opt-in real Cursor test")
	}
	root, data := testProject(t)
	m, err := New(Config{Root: root, DataDir: data, AdapterID: "cursor-agent", CursorBinary: binary, ModelID: "auto"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	u := m.Prepare(ctx, model.Upgrade{ID: "real-cursor-smoke", Title: "临时升级验证", Instructions: "只修改 README.md：将 old readme 替换为 supervisor upgrade smoke verified。不要修改其它文件，不要添加测试。"})
	if u.State != "READY" {
		t.Fatalf("%s\n%s", u.Error, u.Log)
	}
	if !strings.HasPrefix(u.SessionRef, "cursor:") || len(u.Changes) != 1 || u.Changes[0].Path != "README.md" {
		raw, _ := json.Marshal(u)
		t.Fatal(string(raw))
	}
	t.Logf("real Cursor candidate READY; files=%d; sandbox=%s; session bound=%t", len(u.Changes), m.ValidationSandboxName(), u.SessionRef != "")
}

func TestRepositoryCandidateValidation(t *testing.T) {
	root := os.Getenv("WORK_ASSISTANT_TEST_PROJECT")
	if root == "" || os.Getenv("WORK_ASSISTANT_VALIDATION_SANDBOX") != "" {
		t.Skip("opt-in full repository sandbox validation")
	}
	m, err := New(Config{Root: root, DataDir: t.TempDir()}, fakeAdapter{change: func(directory string) error {
		path := filepath.Join(directory, "README.md")
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(body, []byte("\n<!-- disposable supervisor validation -->\n")...), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := m.CheckValidationSandbox(ctx); err != nil {
		t.Fatal(err)
	}
	u := m.Prepare(ctx, model.Upgrade{ID: "repository-smoke", Instructions: "validate disposable candidate"})
	if u.State != "READY" {
		t.Fatalf("%s\n%s", u.Error, u.Log)
	}
	t.Logf("full application candidate READY; sandbox=%s; Go tests/vet, JS syntax and all binaries passed", m.ValidationSandboxName())
}

func TestValidationPreflightRejectsMissingOfflineDependencies(t *testing.T) {
	root, data := testProject(t)
	writeTestFile(t, filepath.Join(root, "go.mod"), "module work-assistant\n\ngo 1.23\n\nrequire example.invalid/not-cached v1.0.0\n", 0o600)
	writeTestFile(t, filepath.Join(root, "cmd/assistant-local/main.go"), "package main\nimport _ \"example.invalid/not-cached\"\nfunc main() {}\n", 0o600)
	m, err := New(testManagerConfig(root, data), fakeAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckValidationSandbox(context.Background()); err == nil || !strings.Contains(err.Error(), "offline toolchain/cache preflight") {
		t.Fatal("unusable offline cache advertised as ready", err)
	}
}
