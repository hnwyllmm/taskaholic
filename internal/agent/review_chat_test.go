package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"work-assistant/internal/model"
)

func TestReviewCannotStartReplacementCodexSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	fake := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a, err := NewCodexAdapter(fake, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"", "workspace:/only-directory", "codex:", "other:session"} {
		result := a.Run(context.Background(), model.RunSpec{TaskGoal: "review question", AgentSessionRef: ref, RequireNativeSession: true}, t.TempDir(), nil, func(Event) { t.Fatal("invalid native reference reached CLI") })
		if result.Err == nil || !strings.Contains(result.Err.Error(), "refusing") {
			t.Fatal(result)
		}
	}
}

func TestReviewRejectsUnexpectedNativeSessionFromCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"wrong-session\"}' '{\"type\":\"turn.completed\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a, err := NewCodexAdapter(fake, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	result := a.Run(context.Background(), model.RunSpec{TaskGoal: "explain delivery", AgentSessionRef: "codex:original-session", RequireNativeSession: true}, dir, nil, func(e Event) {
		if e.Type == "session.bound" {
			t.Fatal("unexpected native identity was emitted")
		}
	})
	if result.Err == nil || !strings.Contains(result.Err.Error(), "different native session") {
		t.Fatal(result)
	}
}
