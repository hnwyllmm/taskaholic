package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"work-assistant/internal/backup"
	"work-assistant/internal/localconfig"
	"work-assistant/internal/localworkspace"
	"work-assistant/internal/server"
	"work-assistant/internal/store"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7337", "HTTP listen address")
	database := flag.String("db", "./data/control.sqlite", "control SQLite path")
	runtimeDatabase := flag.String("runtime-db", "", "optional runtime spool included in the control-owned combined backup")
	runtimeID := flag.String("runtime-id", "", "local runtime ID used for initial helper setup")
	modelID := flag.String("model", "", "model for the initial local helper; existing Agents are never overwritten")
	adapterID := flag.String("adapter", "codex-agent", "adapter for the initial local helper")
	apiToken := flag.String("api-token", os.Getenv("ASSISTANT_API_TOKEN"), "API bearer token")
	runtimeToken := flag.String("runtime-token", os.Getenv("ASSISTANT_RUNTIME_TOKEN"), "runtime bearer token")
	allowRemote := flag.Bool("allow-remote", false, "allow non-loopback HTTP listening")
	noAPIAuth := flag.Bool("no-api-auth", false, "disable browser/control API authentication on a trusted network")
	tlsCert := flag.String("tls-cert", "", "TLS certificate path")
	tlsKey := flag.String("tls-key", "", "TLS private-key path")
	upgradeEnabled := flag.Bool("upgrade-enabled", false, "allow an external supervisor to apply confirmed upgrades")
	supervisorInstance := flag.String("supervisor-instance", "", "opaque supervisor instance used by health checks")
	upgradeValidationSandbox := flag.String("upgrade-validation-sandbox", "unavailable", "candidate validation sandbox maintained by the supervisor")
	verbose := flag.Bool("verbose", false, "enable debug logs")
	flag.Parse()
	if *noAPIAuth {
		*apiToken = ""
		slog.Warn("control API authentication disabled; all reachable network clients can access data and operate tasks")
	}
	if _, err := localconfig.ControlURL(*listen, *allowRemote, *noAPIAuth, *apiToken, *runtimeToken); err != nil {
		fatal(err)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	state, err := store.OpenProtected(*database)
	if err != nil {
		fatal(err)
	}
	defer state.Close()
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	directory, err := backup.Directory(filepath.Dir(*database))
	if err != nil {
		fatal(err)
	}
	backupSources := map[string]backup.Source{"control.sqlite": state}
	if *runtimeDatabase != "" {
		backupSources["runtime.sqlite"] = backup.DatabaseSource{Path: *runtimeDatabase}
	}
	backups, err := backup.New(backup.Config{Directory: directory, Sources: backupSources})
	if err != nil {
		fatal(err)
	}
	if _, err := backups.Capture(ctx, "startup"); err != nil {
		fatal(err)
	}

	application := server.New(server.Config{
		LocalRuntimeID: *runtimeID,
		Listen:         *listen, APIToken: *apiToken, RuntimeToken: *runtimeToken,
		TLSCertFile: *tlsCert, TLSKeyFile: *tlsKey,
		UpgradeEnabled: *upgradeEnabled, UpgradeValidationSandbox: *upgradeValidationSandbox,
		InstanceID: *supervisorInstance, Backups: backups,
	}, state, logger)
	results := make(chan error, 2)
	go func() { results <- application.Run(ctx) }()
	workers := 1
	if *runtimeID != "" {
		workers++
		go func() { results <- localworkspace.Bootstrap(ctx, state, *runtimeID, *modelID, *listen, *adapterID) }()
	}
	var runErr error
	for i := 0; i < workers; i++ {
		if err := <-results; err != nil && runErr == nil {
			runErr = err
		}
		cancel()
	}
	if runErr != nil {
		fatal(runErr)
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "assistantd:", err)
	os.Exit(1)
}
