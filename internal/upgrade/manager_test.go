package upgrade

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

type fakeAdapter struct {
	change  func(string) error
	inspect func(model.RunSpec)
}

type directValidationSandbox struct{}

func (directValidationSandbox) Name() string { return "test-direct" }
func (directValidationSandbox) Wrap(_ string, argv []string) ([]string, error) {
	return argv, nil
}

func testManagerConfig(root, data string) Config {
	return Config{Root: root, DataDir: data, ValidationSandbox: directValidationSandbox{}}
}

func (f fakeAdapter) Name() string                 { return "fake" }
func (f fakeAdapter) Capabilities() map[string]any { return nil }
func (f fakeAdapter) Run(_ context.Context, spec model.RunSpec, directory string, _ <-chan model.Directive, emit func(agent.Event)) agent.Result {
	if f.inspect != nil {
		f.inspect(spec)
	}
	emit(agent.Event{Type: "session.bound", AgentSessionRef: "fake:upgrade-session"})
	if err := f.change(directory); err != nil {
		return agent.Result{Err: err, ExitCode: 1}
	}
	return agent.Result{Output: `{"summary":"added the requested behavior"}`}
}

func writeTestFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func testProject(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(root, "go.mod"), "module work-assistant\n\ngo 1.23\n", 0o600)
	writeTestFile(t, filepath.Join(root, "cmd", "assistant-local", "main.go"), "package main\nfunc main() {}\n", 0o600)
	writeTestFile(t, filepath.Join(root, "internal", "greeting", "greeting.go"), "package greeting\nconst Message = \"old\"\n", 0o600)
	writeTestFile(t, filepath.Join(root, "internal", "store", "store.go"), "package store\n", 0o600)
	writeTestFile(t, filepath.Join(root, "README.md"), "old readme\n", 0o600)
	writeTestFile(t, filepath.Join(root, "bin", "assistant-local"), "old binary\n", 0o700)
	return root, data
}

func TestPrepareApplyAndRollbackCandidate(t *testing.T) {
	root, data := testProject(t)
	manager, err := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		return os.WriteFile(filepath.Join(directory, "README.md"), []byte("new readme\n"), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-safe", Title: "Update docs", Instructions: "update readme", State: "BUILDING"})
	if u.State != "READY" || len(u.Changes) != 1 || u.Changes[0].Path != "README.md" || u.CandidateSHA256 == "" || u.BaseSourceSHA256 == "" || u.BaseBinaries["assistant-local"] == "" || u.SessionRef == "" {
		t.Fatalf("candidate = %+v", u)
	}
	if !strings.Contains(u.Patch, "--- a/README.md") || !strings.Contains(u.Patch, "-old readme") || !strings.Contains(u.Patch, "+new readme") {
		t.Fatalf("review patch does not contain the exact change: %q", u.Patch)
	}
	if !strings.Contains(u.Log, "build -buildvcs=false") {
		t.Fatal("snapshot build must not inherit an ancestor repository's VCS metadata")
	}
	state, err := store.Open(filepath.Join(data, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	backup, err := manager.Backup(context.Background(), state, u)
	if err != nil || backup == "" {
		t.Fatal(backup, err)
	}
	if err = manager.Apply(u); err != nil {
		t.Fatal(err)
	}
	if !manager.IsApplied(u) {
		t.Fatal("applied candidate not recognized")
	}
	if body, _ := os.ReadFile(filepath.Join(root, "README.md")); string(body) != "new readme\n" {
		t.Fatal("source was not installed")
	}
	if err = manager.Rollback(u); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(root, "README.md")); string(body) != "old readme\n" {
		t.Fatal("source was not rolled back")
	}
	if body, _ := os.ReadFile(filepath.Join(root, "bin", "assistant-local")); string(body) != "old binary\n" {
		t.Fatal("binary was not rolled back")
	}
}

func TestPrepareRejectsProtectedInfrastructureChange(t *testing.T) {
	root, data := testProject(t)
	manager, err := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		return os.WriteFile(filepath.Join(directory, "internal", "store", "store.go"), []byte("package store\n// changed\n"), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-protected", Title: "Unsafe", Instructions: "change trust base", State: "BUILDING"})
	if u.State != "FAILED" || u.Error == "" {
		t.Fatalf("protected candidate accepted: %+v", u)
	}
}

func TestPrepareRejectsAddedAssistantLocalSource(t *testing.T) {
	root, data := testProject(t)
	manager, err := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		return os.WriteFile(filepath.Join(directory, "cmd", "assistant-local", "sidecar.go"), []byte("package main\nfunc init() {}\n"), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-local-sidecar", Title: "Unsafe", Instructions: "change local bootstrap", State: "BUILDING"})
	if u.State != "FAILED" || !strings.Contains(u.Error, "protected upgrade infrastructure") {
		t.Fatalf("assistant-local sidecar accepted: %+v", u)
	}
}

func TestCandidateTamperingIsDetected(t *testing.T) {
	root, data := testProject(t)
	manager, _ := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		return os.WriteFile(filepath.Join(directory, "README.md"), []byte("new\n"), 0o600)
	}})
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-tamper", Title: "Update", Instructions: "change", State: "BUILDING"})
	if u.State != "READY" {
		t.Fatal(u.Error)
	}
	writeTestFile(t, filepath.Join(data, "upgrades", u.ID, "candidate", "README.md"), "tampered\n", 0o600)
	if err := manager.VerifyCandidate(u); err == nil {
		t.Fatal("tampered candidate verified")
	}
}

func TestLiveSourceDriftIsDetected(t *testing.T) {
	root, data := testProject(t)
	manager, _ := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		return os.WriteFile(filepath.Join(directory, "README.md"), []byte("new\n"), 0o600)
	}})
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-live-drift", Title: "Update", Instructions: "change", State: "BUILDING"})
	if u.State != "READY" {
		t.Fatal(u.Error)
	}
	writeTestFile(t, filepath.Join(root, "internal", "greeting", "greeting.go"), "package greeting\nconst Message = \"manual change\"\n", 0o600)
	if err := manager.VerifyCandidate(u); err == nil || !strings.Contains(err.Error(), "live source changed") {
		t.Fatalf("live source drift was not rejected: %v", err)
	}
}

func TestPrepareRejectsFilesOutsideInstallableTree(t *testing.T) {
	root, data := testProject(t)
	manager, err := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("new\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(directory, "unreviewed.txt"), []byte("hidden\n"), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-hidden-file", Title: "Unsafe", Instructions: "hide a file", State: "BUILDING"})
	if u.State != "FAILED" || !strings.Contains(u.Error, "unsupported path") {
		t.Fatalf("unsupported candidate file accepted: %+v", u)
	}
}

func TestPrepareRejectsCandidateHardLink(t *testing.T) {
	root, data := testProject(t)
	manager, err := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("new\n"), 0o600); err != nil {
			return err
		}
		return os.Link(filepath.Join(root, "README.md"), filepath.Join(directory, "internal", "greeting", "linked_test.go"))
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-hard-link", Title: "Unsafe", Instructions: "link a file", State: "BUILDING"})
	if u.State != "FAILED" || !strings.Contains(u.Error, "hard links") {
		t.Fatalf("candidate hard link accepted: %+v", u)
	}
}

func TestSeatbeltValidationCannotWriteOrOpenNetworkOutsideJob(t *testing.T) {
	if os.Getenv("WORK_ASSISTANT_VALIDATION_SANDBOX") != "" {
		t.Skip("already running inside the candidate validation sandbox")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Seatbelt integration test")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec is unavailable")
	}
	root, data := testProject(t)
	escape := filepath.Join(t.TempDir(), "validator-escaped")
	localService, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer localService.Close()
	manager, err := New(Config{Root: root, DataDir: data}, fakeAdapter{change: func(directory string) error {
		if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("new\n"), 0o600); err != nil {
			return err
		}
		testSource := `package greeting

import (
	"net"
	"os"
	"testing"
	"time"
)

func TestValidatorSandbox(t *testing.T) {
	if err := os.WriteFile(` + fmt.Sprintf("%q", escape) + `, []byte("escaped"), 0600); err == nil {
		t.Fatal("validator wrote outside its upgrade job")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		listener.Close()
		t.Fatal("validator opened a network socket")
	}
	connection, err := net.DialTimeout("tcp", ` + fmt.Sprintf("%q", localService.Addr().String()) + `, 200*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("validator connected to a local service")
	}
}
`
		return os.WriteFile(filepath.Join(directory, "internal", "greeting", "sandbox_test.go"), []byte(testSource), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-seatbelt", Title: "Sandbox", Instructions: "verify sandbox", State: "BUILDING"})
	if u.State != "READY" {
		t.Fatalf("Seatbelt validation failed: %+v", u)
	}
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("validation escaped its job directory: %v", err)
	}
	if !strings.Contains(u.Log, "[macOS Seatbelt]") {
		t.Fatalf("validation log does not identify sandbox: %q", u.Log)
	}
}

func TestPrepareRejectsSourceMutationDuringValidation(t *testing.T) {
	root, data := testProject(t)
	manager, err := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("new\n"), 0o600); err != nil {
			return err
		}
		testSource := `package greeting

import (
	"os"
	"testing"
)

func TestMutateCandidate(t *testing.T) {
	if err := os.WriteFile(` + fmt.Sprintf("%q", filepath.Join(directory, "README.md")) + `, []byte("changed during test\n"), 0600); err != nil {
		t.Fatal(err)
	}
}
`
		return os.WriteFile(filepath.Join(directory, "internal", "greeting", "mutation_test.go"), []byte(testSource), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-mutating-test", Title: "Unsafe", Instructions: "mutate during tests", State: "BUILDING"})
	if u.State != "FAILED" || !strings.Contains(u.Error, "changed while validation") {
		t.Fatalf("source mutation during validation accepted: %+v", u)
	}
}

func TestUnavailableValidationSandboxFailsClosed(t *testing.T) {
	sandbox := unavailableValidationSandbox{reason: "test platform"}
	if _, err := sandbox.Wrap(t.TempDir(), []string{"true"}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unavailable sandbox did not fail closed: %v", err)
	}
}

func TestRollbackRemovesNewCandidateBinary(t *testing.T) {
	root, data := testProject(t)
	manager, err := New(testManagerConfig(root, data), fakeAdapter{change: func(directory string) error {
		writeTestFile(t, filepath.Join(directory, "cmd", "helper", "main.go"), "package main\nfunc main() {}\n", 0o600)
		return os.WriteFile(filepath.Join(directory, "README.md"), []byte("new\n"), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := manager.Prepare(context.Background(), model.Upgrade{ID: "upgrade-new-binary", Title: "Helper", Instructions: "add helper", State: "BUILDING"})
	if u.State != "READY" {
		t.Fatalf("candidate failed: %+v", u)
	}
	state, err := store.Open(filepath.Join(data, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if _, err = manager.Backup(context.Background(), state, u); err != nil {
		t.Fatal(err)
	}
	if err = manager.Apply(u); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "bin", "helper")); err != nil {
		t.Fatalf("new candidate binary was not installed: %v", err)
	}
	if err = manager.Rollback(u); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "bin", "helper")); !os.IsNotExist(err) {
		t.Fatalf("new candidate binary survived rollback: %v", err)
	}
}
