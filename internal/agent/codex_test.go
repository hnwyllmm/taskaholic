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
	"time"

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
test -z "$ASSISTANT_API_TOKEN" && test -z "$ASSISTANT_RUNTIME_TOKEN" || exit 9
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
	t.Setenv("ASSISTANT_API_TOKEN", "must-not-reach-codex")
	t.Setenv("ASSISTANT_RUNTIME_TOKEN", "must-not-reach-codex")
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
		"--call--", "exec", "approval_policy=\"never\"", "--json", "--sandbox", "read-only", "--model", "model-test", "--output-schema", "sandbox_mode=\"read-only\"",
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
	foundSession, foundMessage, foundApplied, foundUsage := false, false, false, false
	for _, event := range events {
		foundSession = foundSession || (event.Type == "session.bound" && event.AgentSessionRef == "codex:thread-123")
		foundMessage = foundMessage || (event.Stream == "agent" && event.Message == "agent answer")
		foundApplied = foundApplied || (event.Type == "directive.applied" && event.DirectiveID == "directive-1")
		foundUsage = foundUsage || (event.Usage != nil && event.Usage.Provider == "codex" && event.Usage.InputTokens == 1 && event.Usage.OutputTokens == 2)
	}
	if !foundSession || !foundMessage || !foundApplied || !foundUsage {
		t.Fatalf("events = %#v", events)
	}
}

func TestCodexInterruptionStopsChildProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("POSIX process groups")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := NewCodexAdapter(fake, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := a.Run(ctx, model.RunSpec{TaskGoal: "test cancellation"}, dir, nil, func(Event) {})
	if result.Err == nil || time.Since(start) > 3*time.Second {
		t.Fatal("Codex interruption left child output pipes open", result.Err)
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

func TestApprovedDevelopmentPinsSandboxOnNativeResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args")
	binary := filepath.Join(dir, "codex")
	script := `#!/bin/sh
for arg in "$@"; do printf '%s\n' "$arg" >> "$CODEX_FAKE_LOG"; done
printf '%s\n' '{"type":"thread.started","thread_id":"original"}' '{"type":"turn.completed"}'
`
	if e := os.WriteFile(binary, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("CODEX_FAKE_LOG", logPath)
	adapter, e := NewCodexAdapter(binary, "danger-full-access")
	if e != nil {
		t.Fatal(e)
	}
	result := adapter.Run(context.Background(), model.RunSpec{TaskGoal: "implement", AgentSessionRef: "codex:original", RequireNativeSession: true, ExecutionGrant: &model.ExecutionGrant{ReviewID: "approved"}}, dir, nil, func(Event) {})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	raw, e := os.ReadFile(logPath)
	if e != nil {
		t.Fatal(e)
	}
	args := string(raw)
	for _, s := range []string{"resume", "original", `sandbox_mode="workspace-write"`, "sandbox_workspace_write.writable_roots=[]", "sandbox_workspace_write.network_access=false", "sandbox_workspace_write.exclude_slash_tmp=true", "sandbox_workspace_write.exclude_tmpdir_env_var=true"} {
		if !strings.Contains(args, s) {
			t.Fatal("missing sandbox boundary", s)
		}
	}
	if strings.Contains(args, "danger-full-access") {
		t.Fatal("inherited unsafe default")
	}
}

func TestApprovedCodexCapabilitiesChangeOnlyGrantedSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	for _, tc := range []struct {
		name       string
		capability string
		want       []string
		reject     []string
	}{
		{name: "network", capability: "network_access", want: []string{`sandbox_mode="workspace-write"`, "sandbox_workspace_write.network_access=true"}, reject: []string{"danger-full-access"}},
		{name: "host", capability: "host_full_access", want: []string{`sandbox_mode="danger-full-access"`}, reject: []string{"sandbox_workspace_write.network_access="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "args")
			binary := filepath.Join(dir, "codex")
			script := `#!/bin/sh
for arg in "$@"; do printf '%s\n' "$arg" >> "$CODEX_FAKE_LOG"; done
printf '%s\n' '{"type":"thread.started","thread_id":"original"}' '{"type":"turn.completed"}'
`
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_FAKE_LOG", logPath)
			adapter, err := NewCodexAdapter(binary, "read-only")
			if err != nil {
				t.Fatal(err)
			}
			result := adapter.Run(context.Background(), model.RunSpec{TaskGoal: "continue", AgentSessionRef: "codex:original", RequireNativeSession: true, ExecutionGrant: &model.ExecutionGrant{ReviewID: "approved", Capabilities: []string{tc.capability}}}, dir, nil, func(Event) {})
			if result.Err != nil {
				t.Fatal(result.Err)
			}
			raw, _ := os.ReadFile(logPath)
			args := string(raw)
			for _, want := range tc.want {
				if !strings.Contains(args, want) {
					t.Fatalf("missing %q in %s", want, args)
				}
			}
			for _, reject := range tc.reject {
				if strings.Contains(args, reject) {
					t.Fatalf("unexpected %q in %s", reject, args)
				}
			}
		})
	}
}
