package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"work-assistant/internal/model"
)

func TestCodexAdapterCreatesResumesAndAppliesQueuedDirective(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell script")
	}
	directory := t.TempDir()
	logPath := filepath.Join(directory, "arguments.log")
	fake := filepath.Join(directory, "codex-fake")
	script := `#!/bin/sh
printf '%s\n' '--call--' >> "$CODEX_FAKE_LOG"
for arg in "$@"; do printf '%s\n' "$arg" >> "$CODEX_FAKE_LOG"; done
printf '%s\n' '{"type":"thread.started","thread_id":"thread-123"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"agent answer"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}'
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_FAKE_LOG", logPath)
	adapter, err := NewCodexAdapter(fake, "workspace-write")
	if err != nil {
		t.Fatal(err)
	}
	directives := make(chan model.Directive, 1)
	directives <- model.Directive{
		ID: "directive-1", Kind: model.DirectiveKindMessage, Message: "review the result",
	}
	var mutex sync.Mutex
	var events []Event
	result := adapter.Run(context.Background(), model.RunSpec{
		Instructions: "ROLE_POLICY_EVERY_TURN", ReadOnly: true, OutputSchema: json.RawMessage(`{"type":"object"}`),
		TaskTitle: "Implement feature", TaskGoal: "Make it work", ModelID: "model-test",
		AgentSessionRef: "workspace:/not-a-codex-session", Command: []string{"also", "test it"},
	}, directory, directives, func(event Event) {
		mutex.Lock()
		events = append(events, event)
		mutex.Unlock()
	})
	if result.Err != nil || result.ExitCode != 0 || result.Output != "agent answer" {
		t.Fatalf("result = %#v", result)
	}
	arguments, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(arguments)
	for _, expected := range []string{
		"--call--", "exec", "--json", "--sandbox", "read-only", "--model", "model-test", "--output-schema", "sandbox_mode=\"read-only\"",
		"Task: Implement feature", "Goal:\nMake it work", "Additional instructions:\nalso test it",
		"resume", "thread-123", "review the result",
	} {
		if !strings.Contains(log, expected) {
			t.Fatalf("arguments do not contain %q:\n%s", expected, log)
		}
	}
	if strings.Count(log, "--call--") != 2 {
		t.Fatalf("expected a new turn and one resumed turn:\n%s", log)
	}
	if strings.Count(log, "ROLE_POLICY_EVERY_TURN") != 2 || strings.Count(log, "--output-schema") != 2 {
		t.Fatalf("policy/schema missing from a turn: %s", log)
	}
	mutex.Lock()
	defer mutex.Unlock()
	foundSession, foundMessage, foundApplied := false, false, false
	for _, event := range events {
		foundSession = foundSession || (event.Type == "session.bound" && event.AgentSessionRef == "codex:thread-123")
		foundMessage = foundMessage || (event.Stream == "agent" && event.Message == "agent answer")
		foundApplied = foundApplied || (event.Type == "directive.applied" && event.DirectiveID == "directive-1")
	}
	if !foundSession || !foundMessage || !foundApplied {
		t.Fatalf("events = %#v", events)
	}
}

func TestCodexAdapterResumesNativeReference(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell script")
	}
	directory := t.TempDir()
	logPath := filepath.Join(directory, "arguments.log")
	fake := filepath.Join(directory, "codex-fake")
	script := `#!/bin/sh
for arg in "$@"; do printf '%s\n' "$arg" >> "$CODEX_FAKE_LOG"; done
printf '%s\n' '{"type":"thread.started","thread_id":"existing-thread"}'
printf '%s\n' '{"type":"turn.completed"}'
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_FAKE_LOG", logPath)
	adapter, err := NewCodexAdapter(fake, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	result := adapter.Run(context.Background(), model.RunSpec{
		TaskGoal: "continue", AgentSessionRef: "codex:existing-thread", RequireNativeSession: true,
	}, directory, nil, func(Event) {})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	arguments, _ := os.ReadFile(logPath)
	log := string(arguments)
	if !strings.Contains(log, "resume\n") || !strings.Contains(log, "existing-thread\n") || strings.Contains(log, "--sandbox\n") {
		t.Fatalf("unexpected resume arguments:\n%s", log)
	}
}
