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
	"work-assistant/internal/server"
	"work-assistant/internal/store"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7337", "HTTP listen address")
	database := flag.String("db", "./data/control.sqlite", "control SQLite path")
	apiToken := flag.String("api-token", os.Getenv("ASSISTANT_API_TOKEN"), "API bearer token")
	runtimeToken := flag.String("runtime-token", os.Getenv("ASSISTANT_RUNTIME_TOKEN"), "runtime bearer token")
	tlsCert := flag.String("tls-cert", "", "TLS certificate path")
	tlsKey := flag.String("tls-key", "", "TLS private-key path")
	verbose := flag.Bool("verbose", false, "enable debug logs")
	flag.Parse()

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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	directory, err := backup.Directory(filepath.Dir(*database))
	if err != nil {
		fatal(err)
	}
	backups, err := backup.New(backup.Config{Directory: directory, Sources: map[string]backup.Source{"control.sqlite": state}})
	if err != nil {
		fatal(err)
	}
	if _, err := backups.Capture(ctx, "startup"); err != nil {
		fatal(err)
	}

	application := server.New(server.Config{
		Listen: *listen, APIToken: *apiToken, RuntimeToken: *runtimeToken,
		TLSCertFile: *tlsCert, TLSKeyFile: *tlsKey,
		Backups: backups,
	}, state, logger)
	if err := application.Run(ctx); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "assistantd:", err)
	os.Exit(1)
}
