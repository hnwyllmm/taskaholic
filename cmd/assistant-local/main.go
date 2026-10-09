// assistant-local is the single-process personal deployment. The distributed
// assistantd / assistant-runtime binaries remain available independently.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/backup"
	"work-assistant/internal/localworkspace"
	"work-assistant/internal/runtimehost"
	"work-assistant/internal/server"
	"work-assistant/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "assistant-local:", err)
		os.Exit(1)
	}
}
func run() error {
	listen := flag.String("listen", "127.0.0.1:17343", "HTTP listen address; defaults to loopback")
	allowRemote := flag.Bool("allow-remote", false, "allow non-loopback HTTP listening; authentication is required unless --no-api-auth is explicit")
	noAPIAuth := flag.Bool("no-api-auth", false, "disable browser/control API authentication on a trusted network; runtime authentication is unchanged")
	dataDir := flag.String("data", "./data/local", "persistent data directory")
	runtimeID := flag.String("runtime-id", "local", "stable local runtime ID; retain it across restarts")
	modelID := flag.String("model", "", "model for the initial local helper agent; existing agents are never overwritten")
	binary := flag.String("codex-binary", "codex", "installed Codex CLI path")
	adapterID := flag.String("adapter", "codex-agent", "initial execution adapter: codex-agent or cursor-agent")
	extraAdapters := flag.String("extra-adapters", "", "additional execution adapters, comma-separated; does not change existing members or the initial helper")
	cursorBinary := flag.String("cursor-binary", "agent", "installed Cursor Agent CLI path")
	upgradeEnabled := flag.Bool("upgrade-enabled", false, "allow an external supervisor to apply confirmed upgrades")
	supervisorInstance := flag.String("supervisor-instance", "", "opaque supervisor instance used by health checks")
	upgradeValidationSandbox := flag.String("upgrade-validation-sandbox", "unavailable", "candidate validation sandbox maintained by the supervisor")
	flag.Parse()
	apiToken := os.Getenv("ASSISTANT_API_TOKEN")
	runtimeToken := os.Getenv("ASSISTANT_RUNTIME_TOKEN")
	if *noAPIAuth {
		apiToken = ""
		slog.Warn("control API authentication disabled; all reachable network clients can access data and operate tasks")
	}
	controlURL, err := localControlURL(*listen, *allowRemote, *noAPIAuth, apiToken, runtimeToken)
	if err != nil {
		return err
	}
	// Fail before touching presence or the database when a local instance is
	// already listening. This avoids a double-click taking the live runtime offline.
	probe, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("address %s is already in use; open the existing workspace instead: %w", *listen, err)
	}
	if err := probe.Close(); err != nil {
		return err
	}
	adapters, err := localAdapters(*adapterID, *extraAdapters, *binary, *cursorBinary)
	if err != nil {
		return err
	}
	controlPath, spoolPath := filepath.Join(*dataDir, "control.sqlite"), filepath.Join(*dataDir, "runtime.sqlite")
	directory, err := backup.Directory(*dataDir)
	if err != nil {
		return err
	}
	migrationCtx, cancelMigration := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelMigration()
	var migrationPoint backup.Snapshot
	combinedMigration := false
	if _, spoolErr := os.Lstat(spoolPath); spoolErr == nil {
		if migrationPoint, combinedMigration, err = backup.CapturePreMigrationSet(migrationCtx, controlPath, directory, store.SchemaVersion, map[string]backup.Source{"control.sqlite": backup.DatabaseSource{Path: controlPath}, "runtime.sqlite": backup.DatabaseSource{Path: spoolPath}}); err != nil {
			return err
		}
	} else if !os.IsNotExist(spoolErr) {
		return spoolErr
	}
	var state *store.Store
	if combinedMigration {
		state, err = store.OpenProtectedWithRecoveryPoint(controlPath, migrationPoint)
	} else {
		state, err = store.OpenProtected(controlPath)
	}
	if err != nil {
		return err
	}
	defer state.Close()
	spool, err := runtimehost.OpenSpoolProtected(spoolPath)
	if err != nil {
		return err
	}
	defer spool.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	backups, err := backup.New(backup.Config{Directory: directory, Sources: map[string]backup.Source{"control.sqlite": state, "runtime.sqlite": spool}})
	if err != nil {
		return err
	}
	if _, err := backups.Capture(ctx, "startup"); err != nil {
		return fmt.Errorf("required startup recovery point: %w", err)
	}
	control := server.New(server.Config{LocalRuntimeID: *runtimeID, Listen: *listen, APIToken: apiToken, RuntimeToken: runtimeToken, UpgradeEnabled: *upgradeEnabled, UpgradeValidationSandbox: *upgradeValidationSandbox, InstanceID: *supervisorInstance, Backups: backups}, state, nil)
	d, err := runtimehost.New(runtimehost.Config{RuntimeID: *runtimeID, ControlURL: controlURL, Token: runtimeToken, WorkRoot: filepath.Join(*dataDir, "workspaces")}, spool, nil, adapters...)
	if err != nil {
		return err
	}
	results := make(chan error, 3)
	go func() { results <- control.Run(ctx) }()
	go func() { results <- d.Run(ctx) }()
	go func() {
		results <- localworkspace.Bootstrap(ctx, state, *runtimeID, *modelID, *listen, adapters[0].Name())
	}()
	for i := 0; i < 3; i++ {
		e := <-results
		if e != nil {
			err = e
			cancel()
		} else if i < 2 && ctx.Err() != nil {
			cancel()
		}
	}
	return err
}

func localAdapters(primary, extra, codexBinary, cursorBinary string) ([]agent.Adapter, error) {
	names := []string{primary}
	if strings.TrimSpace(extra) != "" {
		names = append(names, strings.Split(extra, ",")...)
	}
	var adapters []agent.Adapter
	seen := make(map[string]bool)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if seen[name] {
			continue
		}
		a, err := localAdapter(name, codexBinary, cursorBinary)
		if err != nil {
			return nil, err
		}
		seen[name] = true
		adapters = append(adapters, a)
	}
	return adapters, nil
}

func localAdapter(adapterID, codexBinary, cursorBinary string) (agent.Adapter, error) {
	switch adapterID {
	case "codex-agent":
		return agent.NewCodexAdapter(codexBinary, "read-only")
	case "cursor-agent":
		return agent.NewCursorAdapter(cursorBinary)
	default:
		return nil, fmt.Errorf("unknown local adapter %q", adapterID)
	}
}

// Keep the package-local seam used by the original bootstrap tests while the
// implementation is shared with the split assistantd process.
func bootstrap(ctx context.Context, state *store.Store, runtimeID, modelID, listen, adapterID string) error {
	return localworkspace.Bootstrap(ctx, state, runtimeID, modelID, listen, adapterID)
}
