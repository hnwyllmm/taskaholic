package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
)

const cursorFixture = `{"type":"system","subtype":"init","session_id":"native-original"}
{"type":"assistant","message":{"content":[{"type":"text","text":"I will inspect the supplied document."},{"type":"thinking","text":"private"}]},"session_id":"native-original"}
{"type":"tool_call","subtype":"started","call_id":"tool-1","tool_call":{"readToolCall":{"args":{"path":"README.md"}}},"session_id":"native-original"}
{"type":"tool_call","subtype":"completed","call_id":"tool-1","tool_call":{"readToolCall":{"args":{"path":"README.md"},"result":{"success":{"content":"# Readme","totalLines":1}}}},"session_id":"native-original"}
{"type":"assistant","message":{"content":[{"type":"text","text":"{\"message\":\"same native context\"}"}]},"session_id":"native-original"}
{"type":"result","subtype":"success","is_error":false,"result":"I will inspect the supplied document.{\"message\":\"same native context\"}","session_id":"native-original"}
`

func TestCursorPublicStreamFinalJSONAndNativeIdentity(t *testing.T) {
	var events []Event
	parsed := scanCursorEvents(strings.NewReader(cursorFixture), "native-original", true, func(e Event) { events = append(events, e) })
	if parsed.Err != nil || parsed.SessionID != "native-original" || parsed.Output != `{"message":"same native context"}` {
		t.Fatal(parsed)
	}
	bound := 0
	var actions []model.Action
	for _, e := range events {
		if e.Type == "session.bound" {
			bound++
			if e.AgentSessionRef != "cursor:native-original" {
				t.Fatal(e)
			}
		}
		if e.Activity != nil {
			actions = append(actions, *e.Activity)
		}
		if strings.Contains(e.Message, "private") {
			t.Fatal("private reasoning leaked")
		}
	}
	if bound != 1 || len(actions) != 4 || actions[1].ID != actions[2].ID || actions[1].State != "RUNNING" || actions[2].State != "COMPLETED" || !strings.Contains(actions[2].Output, "# Readme") {
		t.Fatal(bound, actions)
	}
	var newID string
	scanCursorEvents(strings.NewReader(cursorFixture), "native-original", true, func(e Event) {
		if e.Activity != nil && e.Activity.NativeID == "tool-1" {
			newID = e.Activity.ID
		}
	})
	if newID == actions[1].ID {
		t.Fatal("native tool IDs collide across resumed invocations")
	}
}

func TestCursorRejectsMissingFailedMalformedOrWrongSessionResult(t *testing.T) {
	for _, body := range []string{
		`{"type":"system","subtype":"init","session_id":"wrong-session"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"native-original","result":"not json"}`,
		`{"type":"result","subtype":"error","is_error":true,"session_id":"native-original","result":"Authentication required"}`,
		`{"type":"assistant","session_id":"native-original","message":{"content":[{"type":"text","text":"{}"}]}}`,
		`{"type":"result","subtype":"success","result":"{}"}`,
		`not-json`,
	} {
		t.Run(body[:min(12, len(body))], func(t *testing.T) {
			bound := false
			r := scanCursorEvents(strings.NewReader(body), "native-original", true, func(e Event) {
				if e.Type == "session.bound" {
					bound = true
				}
			})
			if r.Err == nil {
				t.Fatal("invalid stream accepted", r)
			}
			if strings.Contains(body, "wrong-session") && bound {
				t.Fatal("wrong session overwrote original")
			}
		})
	}
}

func fakeCursor(t *testing.T, script string) (*CursorAdapter, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "cursor-test")
	log := filepath.Join(dir, "args.log")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURSOR_TEST_ARGS", log)
	a, err := NewCursorAdapter(binary)
	if err != nil {
		t.Fatal(err)
	}
	return a, dir, log
}
func TestCursorCLIAskSandboxResumesAndRepeatsRoleSchema(t *testing.T) {
	t.Setenv("ASSISTANT_API_TOKEN", "control-test-secret")
	t.Setenv("ASSISTANT_RUNTIME_TOKEN", "runtime-test-secret")
	script := `test -z "$ASSISTANT_API_TOKEN" && test -z "$ASSISTANT_RUNTIME_TOKEN" || exit 9
printf '%s\n' '--call--' "$@" >> "$CURSOR_TEST_ARGS"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"native-original"}' '{"type":"result","subtype":"success","is_error":false,"session_id":"native-original","result":"{\"message\":\"ok\"}"}'
`
	a, dir, log := fakeCursor(t, script)
	directives := make(chan model.Directive, 1)
	directives <- model.Directive{ID: "follow", Kind: model.DirectiveKindMessage, Message: "Explain the previous result"}
	var applied bool
	r := a.Run(context.Background(), model.RunSpec{TaskGoal: "Read supplied input", Instructions: "ROLE_EVERY_TURN", ReadOnly: true, RequireNativeSession: true, AgentSessionRef: "cursor:native-original", ModelID: "auto", OutputSchema: json.RawMessage(`{"type":"object"}`)}, dir, directives, func(e Event) {
		if e.Type == "directive.applied" {
			applied = true
		}
	})
	if r.Err != nil || r.Output != `{"message":"ok"}` || !applied {
		t.Fatal(r, applied)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	for _, needle := range []string{"--mode\nask", "--sandbox\nenabled", "--output-format\nstream-json", "--resume\nnative-original", "--model\nauto", "ROLE_EVERY_TURN", "JSON schema:", "Explain the previous result"} {
		if !strings.Contains(args, needle) {
			t.Fatal("missing argument", needle, args)
		}
	}
	if strings.Count(args, "--resume\nnative-original") != 2 || strings.Count(args, "ROLE_EVERY_TURN") != 2 || strings.Count(args, "JSON schema:") != 2 {
		t.Fatal("lost context/policy on follow-up", args)
	}
	for _, flag := range []string{"--force", "--yolo", "--approve-mcps", "disabled"} {
		if strings.Contains(args, flag) {
			t.Fatal("unexpected permission expansion", flag)
		}
	}
}
func TestCursorInvalidReferenceNeverStartsCLI(t *testing.T) {
	a, dir, log := fakeCursor(t, `printf started > "$CURSOR_TEST_ARGS"`)
	for _, ref := range []string{"", "workspace:/tmp", "codex:other", "cursor:bad/path", "cursor:"} {
		r := a.Run(context.Background(), model.RunSpec{TaskGoal: "Explain", RequireNativeSession: true, AgentSessionRef: ref}, dir, nil, func(Event) {})
		if r.Err == nil {
			t.Fatal("invalid ref accepted", ref)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("invalid stored session started a replacement")
	}
}
func TestCursorInterruptCancelsProcessAndMissingResultFails(t *testing.T) {
	a, dir, _ := fakeCursor(t, `printf '%s\n' '{"type":"system","subtype":"init","session_id":"original"}'
sleep 30
`)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	r := a.Run(ctx, model.RunSpec{TaskGoal: "Explain"}, dir, nil, func(Event) {})
	if r.Err == nil || time.Since(started) > 5*time.Second {
		t.Fatal("interrupt did not terminate command", r, time.Since(started))
	}
}
func TestCursorToolErrorAndCommonSecretMasking(t *testing.T) {
	raw := json.RawMessage(`{"shellToolCall":{"args":{"command":"curl --token fake-secret"},"result":{"success":{"exitCode":2,"output":"api_key=fake-secret"}}}}`)
	a := cursorToolActivity(raw, "completed", "tool1", "scope")
	if a == nil || a.Kind != "command" || a.State != "FAILED" || a.ExitCode == nil || *a.ExitCode != 2 || !a.Redacted || strings.Contains(a.Command+a.Output, "fake-secret") {
		t.Fatal(a)
	}
}

func TestCursorPolicyIsPerWorkspaceAndDoesNotOverwriteExistingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := ensureCursorPolicy(dir); err != nil {
		t.Fatal(err)
	}
	if err := ensureCursorPolicy(dir); err != nil {
		t.Fatal("policy setup is not idempotent", err)
	}
	path := filepath.Join(dir, ".cursor", "cli.json")
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "Shell(*)") || !strings.Contains(string(body), "Mcp(*:*)") || !strings.Contains(string(body), "Write(**)") {
		t.Fatal(string(body), err)
	}
	if err = os.WriteFile(path, []byte("user configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = ensureCursorPolicy(dir); err == nil {
		t.Fatal("overwrote existing policy")
	}
	body, _ = os.ReadFile(path)
	if string(body) != "user configuration" {
		t.Fatal("changed existing configuration")
	}
}
