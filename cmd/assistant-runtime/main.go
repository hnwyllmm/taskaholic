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

	"work-assistant/internal/agent"
	"work-assistant/internal/backup"
	"work-assistant/internal/runtimehost"
)

func main() {
	hostname, _ := os.Hostname()
	runtimeID := flag.String("runtime-id", "runtime-"+hostname, "stable runtime identifier")
	controlURL := flag.String("control-url", "http://127.0.0.1:7337", "control server base URL")
	token := flag.String("token", os.Getenv("ASSISTANT_RUNTIME_TOKEN"), "runtime bearer token")
	spoolPath := flag.String("spool", "./data/runtime.sqlite", "runtime SQLite spool path")
	workRoot := flag.String("work-root", "./data/workspaces", "allowed workspace root")
	codexBinary := flag.String("codex-binary", "codex", "Codex CLI path or executable name")
	codexSandbox := flag.String("codex-sandbox", "workspace-write", "Codex sandbox: read-only, workspace-write, or danger-full-access")
	disableCodex := flag.Bool("disable-codex", false, "do not register the Codex Agent Adapter")
	cursorBinary := flag.String("cursor-binary", "", "opt in to Cursor Agent by providing its executable path")
	disableBackups := flag.Bool("disable-backups", false, "disable this process's backup scheduler when a colocated control process owns combined backups")
	verbose := flag.Bool("verbose", false, "enable debug logs")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	spool, err := runtimehost.OpenSpoolProtected(*spoolPath)
	if err != nil {
		fatal(err)
	}
	defer spool.Close()
	adapters := []agent.Adapter{agent.ExecAdapter{}, agent.StdioAdapter{}}
	if *cursorBinary != "" {
		cursorAdapter, cursorErr := agent.NewCursorAdapter(*cursorBinary)
		if cursorErr != nil {
			fatal(cursorErr)
		}
		adapters = append(adapters, cursorAdapter)
	}
	if !*disableCodex {
		codexAdapter, codexErr := agent.NewCodexAdapter(*codexBinary, *codexSandbox)
		if codexErr != nil {
			logger.Warn("Codex Agent Adapter is unavailable", "error", codexErr)
		} else {
			adapters = append(adapters, codexAdapter)
		}
	}
	daemon, err := runtimehost.New(runtimehost.Config{
		RuntimeID: *runtimeID, ControlURL: *controlURL, Token: *token, WorkRoot: *workRoot,
	}, spool, logger, adapters...)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !*disableBackups {
		directory, err := backup.Directory(filepath.Dir(*spoolPath))
		if err != nil {
			fatal(err)
		}
		backups, err := backup.New(backup.Config{Directory: directory, Sources: map[string]backup.Source{"runtime.sqlite": spool}})
		if err != nil {
			fatal(err)
		}
		if _, err := backups.Capture(ctx, "startup"); err != nil {
			fatal(err)
		}
		backupCtx, cancelBackup := context.WithCancel(ctx)
		backupDone := make(chan struct{})
		go func() { defer close(backupDone); backups.Run(backupCtx) }()
		defer func() { cancelBackup(); <-backupDone }()
	}
	if err := daemon.Run(ctx); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "assistant-runtime:", err)
	os.Exit(1)
}
