package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
	"work-assistant/internal/store"
	"work-assistant/internal/upgrade"
)

type fixtureSandbox struct{}

func (fixtureSandbox) Name() string                                   { return "trusted-test-fixture" }
func (fixtureSandbox) Wrap(_ string, argv []string) ([]string, error) { return argv, nil }

type fixtureBuilder struct{ health string }

func (fixtureBuilder) Name() string                 { return "fake" }
func (fixtureBuilder) Capabilities() map[string]any { return nil }
func (b fixtureBuilder) Run(_ context.Context, _ model.RunSpec, directory string, _ <-chan model.Directive, _ func(agent.Event)) agent.Result {
	err := os.WriteFile(filepath.Join(directory, "internal/greeting/greeting.go"), []byte(fmt.Sprintf("package greeting\nconst Status = %q\n", b.health)), 0o600)
	return agent.Result{Output: `{"summary":"fixture upgrade"}`, Err: err}
}

func fixtureFile(t *testing.T, root, path, body string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func supervisorFixture(t *testing.T, health string) (*childController, *upgrade.Manager, *store.Store) {
	t.Helper()
	root := t.TempDir()
	fixtureFile(t, root, "go.mod", "module work-assistant\n\ngo 1.23\n")
	fixtureFile(t, root, "internal/greeting/greeting.go", "package greeting\nconst Status = \"ready\"\n")
	fixtureFile(t, root, "cmd/assistant-local/main.go", `package main
import("os";"net/http";"encoding/json";"work-assistant/internal/greeting")
func main(){
 var listen,instance string
 for i,a:=range os.Args {if i+1<len(os.Args){switch a{case "--listen":listen=os.Args[i+1];case "--supervisor-instance":instance=os.Args[i+1]}}}
 http.HandleFunc("/health/ready",func(w http.ResponseWriter,r *http.Request){status:=greeting.Status;if status=="upgraded"{status="ready"};json.NewEncoder(w).Encode(map[string]string{"status":status,"instance_id":instance})})
 if err:=http.ListenAndServe(listen,nil);err!=nil{os.Exit(1)}
}
`)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goBinary, "build", "-buildvcs=false", "-o", filepath.Join(root, "bin/assistant-local"), "./cmd/assistant-local")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, out)
	}
	data := filepath.Join(root, "data")
	state, err := store.Open(filepath.Join(data, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	m, err := upgrade.New(upgrade.Config{Root: root, DataDir: data, GoBinary: goBinary, ValidationSandbox: fixtureSandbox{}}, fixtureBuilder{health: health})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := l.Addr().String()
	l.Close()
	c := newChildController(options{root: root, dataDir: data, listen: listen, runtimeID: "fixture", instanceID: "fixture-instance", adapterID: "cursor-agent", cursorBinary: "agent"})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitHealthy(context.Background(), listen, c.options.instanceID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return c, m, state
}

func fixtureInstalling(t *testing.T, m *upgrade.Manager, state *store.Store) model.Upgrade {
	t.Helper()
	ctx := context.Background()
	u, err := state.CreateUpgrade(ctx, "fixture", "test controlled install")
	if err != nil {
		t.Fatal(err)
	}
	u.State = "BUILDING"
	u, err = state.ChangeUpgrade(ctx, u, u.Version, "UpgradeBuildStarted")
	if err != nil {
		t.Fatal(err)
	}
	u = m.Prepare(ctx, u)
	if u.State != "READY" {
		t.Fatalf("%s\n%s", u.Error, u.Log)
	}
	u, err = state.ChangeUpgrade(ctx, u, u.Version, "UpgradeCandidateReady")
	if err != nil {
		t.Fatal(err)
	}
	u, err = state.RequestUpgradeInstall(ctx, u.ID, u.Version, u.CandidateSHA256)
	if err != nil {
		t.Fatal(err)
	}
	u, err = state.BeginUpgradeInstall(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSupervisorInstallAndFailedHealthRollback(t *testing.T) {
	for _, status := range []string{"upgraded", "broken"} {
		t.Run(status, func(t *testing.T) {
			c, m, state := supervisorFixture(t, status)
			u := fixtureInstalling(t, m, state)
			install(context.Background(), state, m, c, u, c.options.listen)
			got, err := state.GetUpgrade(context.Background(), u.ID)
			want := "SUCCEEDED"
			if status == "broken" {
				want = "ROLLED_BACK"
			}
			if err != nil || got.State != want || got.BackupDir == "" {
				t.Fatalf("%+v %v", got, err)
			}
			if err := waitHealthy(context.Background(), c.options.listen, c.options.instanceID, 3*time.Second); err != nil {
				t.Fatal(err)
			}
			if maintenance, err := state.Maintenance(context.Background()); err != nil || maintenance != "" {
				t.Fatal(maintenance, err)
			}
			t.Logf("upgrade outcome=%s; healthy child; maintenance cleared; backup recorded", got.State)
		})
	}
}

func TestSupervisorRestartsExitedChildAndRecoversAppliedInstall(t *testing.T) {
	c, m, state := supervisorFixture(t, "upgraded")
	c.mu.Lock()
	pid := c.command.Process.Pid
	command, done := c.command, c.done
	c.mu.Unlock()
	if err := signalChild(command, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit")
	}
	if err := c.Ensure(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	newPID := c.command.Process.Pid
	c.mu.Unlock()
	if newPID == pid {
		t.Fatal("child did not restart")
	}
	if err := waitHealthy(context.Background(), c.options.listen, c.options.instanceID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	u := fixtureInstalling(t, m, state)
	if _, err := m.Backup(context.Background(), state, u); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(u); err != nil {
		t.Fatal(err)
	}
	// Simulate a supervisor restart after applying files but before recording
	// success. It must use the durable candidate, not ask an Agent to rebuild it.
	recovered := newChildController(c.options)
	defer recovered.Stop(context.Background())
	install(context.Background(), state, m, recovered, u, c.options.listen)
	got, err := state.GetUpgrade(context.Background(), u.ID)
	if err != nil || got.State != "SUCCEEDED" {
		t.Fatal(got, err)
	}
}

func TestSupervisorPassesCursorAndExplicitExposureFlags(t *testing.T) {
	c := newChildController(options{adapterID: "cursor-agent", extraAdapters: "codex-agent", codexBinary: "/custom path/codex", cursorBinary: "/custom path/agent", allowRemote: true, noAPIAuth: true, listen: "0.0.0.0:17343"})
	args := c.childArgs()
	joined := strings.Join(args, "\x00")
	for _, want := range []string{"--adapter\x00cursor-agent", "--extra-adapters\x00codex-agent", "--codex-binary\x00/custom path/codex", "--cursor-binary\x00/custom path/agent", "--allow-remote", "--no-api-auth", "--upgrade-enabled"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing", want)
		}
	}
	defaults := newChildController(options{})
	joined = strings.Join(defaults.childArgs(), " ")
	if strings.Contains(joined, "--allow-remote") || strings.Contains(joined, "--no-api-auth") || strings.Contains(joined, "--extra-adapters") {
		t.Fatal("insecure default")
	}
}

func TestRecoveryFailureKeepsDurableMaintenanceAndDoesNotStartChild(t *testing.T) {
	c, m, state := supervisorFixture(t, "upgraded")
	u := fixtureInstalling(t, m, state)
	if _, err := m.Backup(context.Background(), state, u); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(u); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, filepath.Join(c.options.dataDir, "upgrades", u.ID, "candidate"), "internal/greeting/greeting.go", "tampered after restart")
	recovered := newChildController(c.options)
	defer recovered.Stop(context.Background())
	if err := install(context.Background(), state, m, recovered, u, c.options.listen); err == nil {
		t.Fatal("unverified recovery accepted")
	}
	got, err := state.GetUpgrade(context.Background(), u.ID)
	if err != nil || got.State != "INSTALLING" || !strings.Contains(got.Error, "恢复受阻") {
		t.Fatal(got, err)
	}
	if maintenance, err := state.Maintenance(context.Background()); err != nil || maintenance != u.ID {
		t.Fatal("maintenance cleared", maintenance, err)
	}
	if recovered.command != nil {
		t.Fatal("mixed release started")
	}
}
