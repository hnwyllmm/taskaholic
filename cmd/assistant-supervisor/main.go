// assistant-supervisor keeps the personal deployment running and owns the
// trusted install/restart/rollback boundary for explicitly approved upgrades.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"work-assistant/internal/backup"
	"work-assistant/internal/id"
	"work-assistant/internal/localconfig"
	"work-assistant/internal/model"
	"work-assistant/internal/store"
	"work-assistant/internal/upgrade"
)

// assistant-local creates and verifies a recovery point before it starts the
// HTTP server. That work scales with the live databases and retention backlog,
// so a short process-start timeout eventually turns successful backup work into
// an endless restart loop. Keep this distinct from the much shorter health
// checks used while installing an already-drained upgrade candidate.
const initialWorkspaceHealthTimeout = 90 * time.Second

type options struct {
	root              string
	dataDir           string
	listen            string
	controlURL        string
	runtimeID         string
	modelID           string
	codexBinary       string
	cursorBinary      string
	adapterID         string
	extraAdapters     string
	allowRemote       bool
	noAPIAuth         bool
	goBinary          string
	nodeBinary        string
	instanceID        string
	validationSandbox string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "assistant-supervisor:", err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.StringVar(&o.root, "root", ".", "application source and binary root")
	flag.StringVar(&o.dataDir, "data", "./data/local", "persistent data directory")
	flag.StringVar(&o.listen, "listen", "127.0.0.1:17343", "child HTTP address")
	flag.StringVar(&o.runtimeID, "runtime-id", "local", "stable local runtime ID")
	flag.StringVar(&o.modelID, "model", "", "model for the local helper and upgrade agent")
	flag.StringVar(&o.codexBinary, "codex-binary", "codex", "installed Codex CLI path")
	flag.StringVar(&o.cursorBinary, "cursor-binary", "agent", "installed Cursor Agent CLI path")
	flag.StringVar(&o.adapterID, "adapter", "codex-agent", "local helper and default upgrade adapter: codex-agent or cursor-agent")
	flag.StringVar(&o.extraAdapters, "extra-adapters", "", "additional execution adapters, comma-separated; retain the primary adapter and existing sessions")
	flag.BoolVar(&o.allowRemote, "allow-remote", false, "explicitly allow non-loopback listening with runtime authentication")
	flag.BoolVar(&o.noAPIAuth, "no-api-auth", false, "explicitly disable browser API authentication on a trusted network")
	flag.StringVar(&o.goBinary, "go-binary", "", "Go compiler used to validate candidates")
	flag.StringVar(&o.nodeBinary, "node-binary", "", "Node.js used to syntax-check browser code")
	flag.Parse()
	var err error
	if o.root, err = filepath.Abs(o.root); err != nil {
		return err
	}
	if o.dataDir, err = filepath.Abs(o.dataDir); err != nil {
		return err
	}
	if o.root == string(filepath.Separator) || o.dataDir == string(filepath.Separator) {
		return errors.New("root and data directory must not be the filesystem root")
	}
	if o.controlURL, err = localconfig.ControlURL(o.listen, o.allowRemote, o.noAPIAuth, os.Getenv("ASSISTANT_API_TOKEN"), os.Getenv("ASSISTANT_RUNTIME_TOKEN")); err != nil {
		return err
	}
	if err := os.MkdirAll(o.dataDir, 0o700); err != nil {
		return err
	}
	lock, err := acquireSupervisorLock(filepath.Join(o.dataDir, "supervisor.lock"))
	if err != nil {
		return err
	}
	defer releaseSupervisorLock(lock)
	probe, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("address %s is already in use; stop the existing direct process or open it instead: %w", o.listen, err)
	}
	if err := probe.Close(); err != nil {
		return err
	}
	o.instanceID = id.New("supervisor")
	if o.goBinary == "" {
		for _, candidate := range []string{
			filepath.Join(o.dataDir, ".toolchains", "go", "bin", "go"),
			filepath.Join(runtime.GOROOT(), "bin", "go"),
		} {
			if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				o.goBinary = candidate
				break
			}
		}
	}
	state, err := store.OpenProtected(filepath.Join(o.dataDir, "control.sqlite"))
	if err != nil {
		return err
	}
	defer state.Close()
	manager, err := upgrade.New(upgrade.Config{RuntimeID: o.runtimeID, Root: o.root, DataDir: o.dataDir, GoBinary: o.goBinary, NodeBinary: o.nodeBinary, ModelID: o.modelID, CodexBinary: o.codexBinary, CursorBinary: o.cursorBinary, AdapterID: o.adapterID}, nil)
	if err != nil {
		return err
	}
	o.validationSandbox = manager.ValidationSandboxName()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := manager.CheckValidationSandbox(ctx); err != nil {
		return err
	}
	child := newChildController(o)
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer stopCancel()
		_ = child.Stop(stopCtx)
	}()
	// An interrupted installation may have replaced only some files. Recover
	// that durable transaction before exposing any potentially mixed release.
	items, err := state.ListUpgrades(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.State != "INSTALLING" {
			continue
		}
		if err := install(ctx, state, manager, child, item, o.listen); err != nil {
			return err
		}
		current, err := state.GetUpgrade(ctx, item.ID)
		if err != nil {
			return err
		}
		if current.State != "SUCCEEDED" && current.State != "ROLLED_BACK" {
			return fmt.Errorf("interrupted upgrade needs recovery before startup: %s: %s", item.ID, current.Error)
		}
	}
	if err := child.Start(); err != nil {
		return err
	}
	if err := waitHealthy(ctx, o.listen, o.instanceID, initialWorkspaceHealthTimeout); err != nil {
		return fmt.Errorf("start local workspace: %w", err)
	}

	var building atomic.Bool
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := child.Ensure(); err != nil {
				slog.Error("restart local workspace", "error", err)
			}
			items, err := state.ListUpgrades(ctx)
			if err != nil {
				slog.Error("read upgrades", "error", err)
				continue
			}
			for _, item := range items {
				switch item.State {
				case "QUEUED", "BUILDING":
					if building.CompareAndSwap(false, true) {
						go prepare(ctx, state, manager, item, &building)
					}
					goto nextTick
				case "WAITING_IDLE":
					installing, beginErr := state.BeginUpgradeInstall(ctx, item.ID, child.SupportsControlRestart())
					if beginErr == nil {
						if err := install(ctx, state, manager, child, installing, o.listen); err != nil {
							return err
						}
					} else if !errors.Is(beginErr, model.ErrConflict) {
						slog.Error("begin upgrade install", "upgrade_id", item.ID, "error", beginErr)
					}
					goto nextTick
				case "INSTALLING":
					if err := install(ctx, state, manager, child, item, o.listen); err != nil {
						return err
					}
					goto nextTick
				}
			}
		nextTick:
		}
	}
}

func prepare(parent context.Context, state *store.Store, manager *upgrade.Manager, item model.Upgrade, building *atomic.Bool) {
	defer building.Store(false)
	item.State = "BUILDING"
	item.Error = ""
	claimed, err := state.ChangeUpgrade(parent, item, item.Version, "UpgradeBuildStarted")
	if err != nil {
		if !errors.Is(err, model.ErrConflict) {
			slog.Error("claim upgrade build", "upgrade_id", item.ID, "error", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Minute)
	defer cancel()
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, getErr := state.GetUpgrade(ctx, claimed.ID)
				if getErr == nil && (current.State != "BUILDING" || current.Version != claimed.Version) {
					cancel()
					return
				}
			}
		}
	}()
	result := manager.Prepare(ctx, claimed)
	event := "UpgradeCandidateReady"
	if result.State == "FAILED" {
		event = "UpgradeBuildFailed"
	}
	if _, err = state.ChangeUpgrade(parent, result, claimed.Version, event); err != nil && !errors.Is(err, model.ErrConflict) {
		slog.Error("save upgrade build", "upgrade_id", item.ID, "error", err)
	}
}

func install(ctx context.Context, state *store.Store, manager *upgrade.Manager, child *childController, item model.Upgrade, listen string) error {
	child.mu.Lock()
	recovering := child.command == nil
	child.mu.Unlock()
	if err := manager.VerifyCandidate(item); err != nil {
		if recovering {
			return recordRecoveryFailure(state, item, err)
		}
		return state.FinishUpgrade(ctx, item.ID, "FAILED", err.Error(), "")
	}
	if manager.IsApplied(item) {
		if err := child.Ensure(); err == nil && waitHealthy(ctx, listen, child.options.instanceID, 20*time.Second) == nil {
			return state.FinishUpgrade(ctx, item.ID, "SUCCEEDED", "", manager.BackupPath(item.ID))
		}
	}
	controlOnly := item.RestartScope == model.UpgradeRestartControl && child.SupportsControlRestart()
	stopChild := child.Stop
	startChild := child.Start
	if controlOnly {
		stopChild = child.StopControl
		startChild = child.StartControl
	}
	stopped := false
	stop := func() error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		err := stopChild(stopCtx)
		stopped = true // Stop either completed or escalated to SIGKILL before returning.
		return err
	}
	resumeOld := func() error {
		if !stopped {
			return nil
		}
		if err := startChild(); err != nil {
			return err
		}
		return waitHealthy(ctx, listen, child.options.instanceID, initialWorkspaceHealthTimeout)
	}
	// With active Runs, close the control connection before copying either
	// database. The Runtime keeps Agents alive and spools new events, while no
	// event can be acknowledged by control between the two SQLite snapshots.
	if controlOnly {
		if err := stop(); err != nil {
			if resumeErr := resumeOld(); resumeErr != nil || recovering {
				return recordRecoveryFailure(state, item, fmt.Errorf("stop control: %v; resume: %v", err, resumeErr))
			}
			return state.FinishUpgrade(ctx, item.ID, "FAILED", "stop control: "+err.Error(), "")
		}
	}
	backup, err := manager.Backup(ctx, state, item)
	if err != nil {
		resumeErr := resumeOld()
		if recovering || resumeErr != nil {
			return recordRecoveryFailure(state, item, fmt.Errorf("backup: %v; resume: %v", err, resumeErr))
		}
		return state.FinishUpgrade(ctx, item.ID, "FAILED", "backup: "+err.Error(), "")
	}
	if !stopped {
		err = stop()
	}
	if err == nil {
		err = manager.Apply(item)
	}
	if err == nil && !manager.IsApplied(item) {
		err = errors.New("installed release does not match the approved candidate")
	}
	if err == nil {
		err = startChild()
	}
	if err == nil {
		err = waitHealthy(ctx, listen, child.options.instanceID, 20*time.Second)
	}
	if err == nil {
		return state.FinishUpgrade(ctx, item.ID, "SUCCEEDED", "", backup)
	}
	installErr := err
	rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 12*time.Second)
	rollbackErr := stopChild(rollbackCtx)
	rollbackCancel()
	if rollbackErr == nil {
		rollbackErr = manager.Rollback(item)
	}
	if rollbackErr == nil {
		rollbackErr = startChild()
	}
	if rollbackErr == nil {
		rollbackErr = waitHealthy(ctx, listen, child.options.instanceID, 20*time.Second)
	}
	if rollbackErr == nil {
		return state.FinishUpgrade(ctx, item.ID, "ROLLED_BACK", installErr.Error(), backup)
	}
	return recordRecoveryFailure(state, item, fmt.Errorf("install: %v; rollback: %v", installErr, rollbackErr))
}

// Keep INSTALLING + maintenance durable when neither release is verified.
// Marking it FAILED and clearing maintenance would let the next systemd restart
// boot a mixed/broken tree as if there had never been an interrupted install.
func recordRecoveryFailure(state *store.Store, item model.Upgrade, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := state.GetUpgrade(ctx, item.ID)
	if err == nil {
		current.Error = "升级恢复受阻；保留维护锁并停止服务：" + cause.Error()
		_, err = state.ChangeUpgrade(ctx, current, current.Version, "UpgradeRecoveryBlocked")
	}
	return fmt.Errorf("upgrade %s requires recovery: %w (record: %v)", item.ID, cause, err)
}

func waitHealthy(ctx context.Context, listen, instanceID string, timeout time.Duration) error {
	base, err := localconfig.HealthBaseURL(listen)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: time.Second}
	return waitHealthyWithClient(ctx, client, base+"/health/ready", instanceID, timeout)
}

func waitHealthyWithClient(ctx context.Context, client *http.Client, url, instanceID string, timeout time.Duration) error {
	healthCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := "no response"
	for {
		request, _ := http.NewRequestWithContext(healthCtx, http.MethodGet, url, nil)
		response, err := client.Do(request)
		if err == nil {
			var status struct {
				Status     string `json:"status"`
				InstanceID string `json:"instance_id"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&status)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK && decodeErr == nil && status.Status == "ready" && status.InstanceID == instanceID {
				return nil
			}
			last = fmt.Sprintf("status=%d ready=%q instance=%q decode=%v", response.StatusCode, status.Status, status.InstanceID, decodeErr)
		} else {
			last = err.Error()
		}
		select {
		case <-healthCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("new release did not become ready: %s", last)
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func acquireSupervisorLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another assistant-supervisor is already running: %w", err)
	}
	if err = file.Truncate(0); err == nil {
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		_, err = fmt.Fprintf(file, "%d\n", os.Getpid())
	}
	if err != nil {
		releaseSupervisorLock(file)
		return nil, err
	}
	return file, nil
}

func releaseSupervisorLock(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

type childController struct {
	mu              sync.Mutex
	options         options
	command         *exec.Cmd // assistantd in split mode; assistant-local in legacy mode
	done            chan error
	runtimeCommand  *exec.Cmd
	runtimeDone     chan error
	split           bool
	stopping        bool
	controlStopping bool
	runtimeStopping bool
}

func newChildController(o options) *childController {
	c := &childController{options: o}
	c.split = c.splitAvailable()
	return c
}

func (c *childController) SupportsControlRestart() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.split
}

func (c *childController) childArgs() []string {
	args := []string{
		"--data", c.options.dataDir,
		"--runtime-id", c.options.runtimeID,
		"--listen", c.options.listen,
		"--codex-binary", c.options.codexBinary,
		"--cursor-binary", c.options.cursorBinary,
		"--adapter", c.options.adapterID,
		"--model", c.options.modelID,
		"--supervisor-instance", c.options.instanceID,
		"--upgrade-validation-sandbox", c.options.validationSandbox,
		"--upgrade-enabled",
	}
	if c.options.allowRemote {
		args = append(args, "--allow-remote")
	}
	if c.options.extraAdapters != "" {
		args = append(args, "--extra-adapters", c.options.extraAdapters)
	}
	if c.options.noAPIAuth {
		args = append(args, "--no-api-auth")
	}
	return args
}

func (c *childController) controlArgs() []string {
	args := []string{
		"--db", filepath.Join(c.options.dataDir, "control.sqlite"),
		"--runtime-db", filepath.Join(c.options.dataDir, "runtime.sqlite"),
		"--runtime-id", c.options.runtimeID,
		"--listen", c.options.listen,
		"--adapter", c.options.adapterID,
		"--model", c.options.modelID,
		"--supervisor-instance", c.options.instanceID,
		"--upgrade-validation-sandbox", c.options.validationSandbox,
		"--upgrade-enabled",
	}
	if c.options.allowRemote {
		args = append(args, "--allow-remote")
	}
	if c.options.noAPIAuth {
		args = append(args, "--no-api-auth")
	}
	return args
}

func (c *childController) runtimeArgs() []string {
	return []string{
		"--runtime-id", c.options.runtimeID,
		"--control-url", c.options.controlURL,
		"--spool", filepath.Join(c.options.dataDir, "runtime.sqlite"),
		"--work-root", filepath.Join(c.options.dataDir, "workspaces"),
		"--codex-binary", c.options.codexBinary,
		"--codex-sandbox", "read-only",
		"--cursor-binary", c.options.cursorBinary,
		"--disable-backups",
	}
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func (c *childController) splitAvailable() bool {
	return executable(filepath.Join(c.options.root, "bin", "assistantd")) &&
		executable(filepath.Join(c.options.root, "bin", "assistant-runtime"))
}

func waitRuntimeSpool(path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var last error
	for {
		if last = backup.VerifySQLite(ctx, path); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("runtime spool did not become ready: %w", last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func childCommand(binary string, args []string) *exec.Cmd {
	var command *exec.Cmd
	if runtime.GOOS == "darwin" {
		// Recent macOS releases can SIGKILL a locally ad-hoc-signed Mach-O
		// launched directly by another Go binary in restricted app contexts.
		// An Apple-signed shell that remains as the process-group leader avoids
		// that policy edge while keeping every argument out of shell parsing.
		script := `"$@" &
child=$!
trap 'kill -TERM "$child" 2>/dev/null; wait "$child"; exit 0' TERM INT
wait "$child"`
		arguments := append([]string{"-c", script, "assistant-supervisor-child", binary}, args...)
		command = exec.Command("/bin/sh", arguments...)
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		command = exec.Command(binary, args...)
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	return command
}

func (c *childController) startOneLocked(kind, binary string, args []string) error {
	if kind == "runtime" {
		if c.runtimeCommand != nil {
			return nil
		}
	} else if c.command != nil {
		return nil
	}
	command := childCommand(binary, args)
	command.Dir = c.options.root
	command.Env = os.Environ()
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	slog.Info("local workspace process started", "component", kind, "pid", command.Process.Pid, "instance", c.options.instanceID)
	done := make(chan error, 1)
	if kind == "runtime" {
		c.runtimeCommand, c.runtimeDone, c.runtimeStopping = command, done, false
	} else {
		c.command, c.done, c.controlStopping = command, done, false
	}
	go func() {
		err := command.Wait()
		c.mu.Lock()
		intentional := c.stopping
		if kind == "runtime" {
			if c.runtimeCommand == command {
				c.runtimeCommand = nil
			}
			intentional = intentional || c.runtimeStopping
		} else {
			if c.command == command {
				c.command = nil
			}
			intentional = intentional || c.controlStopping
		}
		c.mu.Unlock()
		done <- err
		if !intentional {
			if err == nil {
				slog.Warn("local workspace component stopped unexpectedly; supervisor will restart it", "component", kind)
			} else {
				slog.Error("local workspace component exited; supervisor will restart it", "component", kind, "error", err)
			}
		} else {
			slog.Info("local workspace process stopped", "component", kind, "error", err)
		}
	}()
	return nil
}

func (c *childController) startLocked() error {
	if c.split {
		// Create/open the spool first so assistantd's mandatory verified recovery
		// point can include both databases. The runtime reconnects until
		// the control listener becomes available.
		if err := c.startOneLocked("runtime", filepath.Join(c.options.root, "bin", "assistant-runtime"), c.runtimeArgs()); err != nil {
			return err
		}
		if err := waitRuntimeSpool(filepath.Join(c.options.dataDir, "runtime.sqlite"), 10*time.Second); err != nil {
			return err
		}
		return c.startOneLocked("control", filepath.Join(c.options.root, "bin", "assistantd"), c.controlArgs())
	}
	return c.startOneLocked("combined", filepath.Join(c.options.root, "bin", "assistant-local"), c.childArgs())
}

func (c *childController) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.split = c.splitAvailable()
	c.stopping, c.controlStopping, c.runtimeStopping = false, false, false
	return c.startLocked()
}

func (c *childController) Ensure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopping {
		return nil
	}
	// Do not switch a running legacy process merely because new binaries were
	// installed. Split mode is selected only at an intentional Start boundary.
	return c.startLocked()
}

func (c *childController) stopOne(ctx context.Context, runtimeChild bool) error {
	c.mu.Lock()
	command, done := c.command, c.done
	if runtimeChild {
		command, done = c.runtimeCommand, c.runtimeDone
		c.runtimeStopping = true
	} else {
		c.controlStopping = true
	}
	if command == nil {
		c.mu.Unlock()
		return nil
	}
	err := signalChild(command, syscall.SIGTERM)
	c.mu.Unlock()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		_ = signalChild(command, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

// RestartControl leaves assistant-runtime and every active Agent subprocess
// alive. The runtime reconnect loop buffers events in runtime.sqlite until the
// new control process is ready.
func (c *childController) StopControl(ctx context.Context) error {
	c.mu.Lock()
	split := c.split
	c.mu.Unlock()
	if !split {
		return c.Stop(ctx)
	}
	return c.stopOne(ctx, false)
}

func (c *childController) StartControl() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.split {
		c.stopping, c.controlStopping = false, false
		return c.startLocked()
	}
	c.controlStopping = false
	return c.startOneLocked("control", filepath.Join(c.options.root, "bin", "assistantd"), c.controlArgs())
}

func (c *childController) RestartControl(ctx context.Context) error {
	if err := c.StopControl(ctx); err != nil {
		return err
	}
	return c.StartControl()
}

func (c *childController) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.stopping = true
	split := c.split
	c.mu.Unlock()
	var first error
	if err := c.stopOne(ctx, false); first == nil {
		first = err
	}
	if split {
		if err := c.stopOne(ctx, true); first == nil {
			first = err
		}
	}
	return first
}

func signalChild(command *exec.Cmd, signal syscall.Signal) error {
	if command.SysProcAttr != nil && command.SysProcAttr.Setpgid {
		if err := syscall.Kill(-command.Process.Pid, signal); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	return command.Process.Signal(signal)
}
