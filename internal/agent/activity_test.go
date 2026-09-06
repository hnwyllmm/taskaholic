package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexStructuredActionLifecycleAndResumeScope(t *testing.T) {
	fixture := `{"type":"thread.started","thread_id":"native-original"}
{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"go test ./...","status":"in_progress","aggregated_output":""}}
{"type":"item.updated","item":{"id":"item_0","type":"command_execution","command":"go test ./...","status":"in_progress","aggregated_output":"ok package-one\n"}}
{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"go test ./...","status":"completed","aggregated_output":"ok package-one\nok package-two\n","exit_code":0}}
{"type":"item.completed","item":{"id":"private","type":"reasoning","text":"private reasoning must not be displayed"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"{\"message\":\"final result\",\"token\":\"result-must-stay-original\"}"}}
{"type":"turn.completed"}
`
	var events []Event
	result := scanCodexEvents(strings.NewReader(fixture), func(e Event) { events = append(events, e) })
	if result.Err != nil || result.SessionID != "native-original" || !strings.Contains(result.Output, "result-must-stay-original") {
		t.Fatal(result)
	}
	var actionEvents []Event
	for _, e := range events {
		if strings.Contains(e.Message, "private reasoning") {
			t.Fatal("leaked reasoning")
		}
		if e.Activity != nil {
			actionEvents = append(actionEvents, e)
		}
	}
	if len(actionEvents) != 4 {
		t.Fatal("unexpected observations", actionEvents)
	}
	first, update, finish := actionEvents[0].Activity, actionEvents[1].Activity, actionEvents[2].Activity
	if first.ID != update.ID || first.ID != finish.ID || first.NativeID != "item_0" || first.State != "RUNNING" || finish.State != "COMPLETED" || finish.ExitCode == nil || *finish.ExitCode != 0 {
		t.Fatal(first, update, finish)
	}
	if update.Output != "ok package-one\n" || finish.Output != "ok package-one\nok package-two\n" {
		t.Fatal("output snapshot corrupted")
	}
	if !strings.Contains(actionEvents[2].Message, "go test ./...") || !strings.Contains(actionEvents[2].Message, "ok package-two") {
		t.Fatal("legacy log consumers lost command/output")
	}
	if strings.Contains(actionEvents[3].Activity.Output, "result-must-stay-original") {
		t.Fatal("display output must be masked separately from business result")
	}
	var resumedID string
	scanCodexEvents(strings.NewReader(fixture), func(e Event) {
		if e.Activity != nil && resumedID == "" {
			resumedID = e.Activity.ID
		}
	})
	if resumedID == first.ID {
		t.Fatal("provider item ID reused across CLI invocations")
	}
}

func TestCodexActionTypes(t *testing.T) {
	for _, c := range []struct{ raw, kind, state, needle string }{
		{`{"id":"x","type":"command_execution","command":"false","exit_code":1}`, "command", "FAILED", "false"},
		{`{"id":"x","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"query":"sql"},"result":{"content":"found"}}`, "tool", "COMPLETED", "sql"},
		{`{"id":"x","type":"mcp_tool_call","error":{"message":"tool unavailable"}}`, "tool", "FAILED", "tool unavailable"},
		{`{"id":"x","type":"file_change","changes":[{"path":"design.md","kind":"update"}]}`, "file_change", "COMPLETED", "design.md"},
		{`{"id":"x","type":"todo_list","items":[{"text":"verify","completed":false}]}`, "plan", "COMPLETED", "verify"},
		{`{"id":"x","type":"web_search","query":"official documentation"}`, "search", "COMPLETED", "official documentation"},
	} {
		t.Run(c.kind+"/"+c.state, func(t *testing.T) {
			a := codexActivity(json.RawMessage(c.raw), "item.completed", "scope")
			if a == nil || a.Kind != c.kind || a.State != c.state || !strings.Contains(a.Command+a.Details+a.Output+a.Error, c.needle) {
				t.Fatal(a)
			}
		})
	}
	for _, raw := range []string{`{"id":"x","type":"reasoning","text":"private"}`, `{"id":"x","type":"unknown"}`, `{"type":"command_execution"}`, `not-json`} {
		if a := codexActivity(json.RawMessage(raw), "item.started", "scope"); a != nil {
			t.Fatal("unexpected public action", a)
		}
	}
}
