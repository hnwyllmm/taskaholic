package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
)

// Explicitly opt in: two small real model calls using the installed login. No
// production Work Assistant tasks, checkouts, bindings or databases are touched.
func TestInstalledCodexCatalogAndNativeSession(t *testing.T) {
	if os.Getenv("ASSISTANT_TEST_CODEX") != "1" {
		t.Skip("set ASSISTANT_TEST_CODEX=1 for installed CLI integration")
	}
	a, err := NewCodexAdapter("codex", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	models, err := a.ListModels(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatal("installed model catalog", err)
	}
	t.Logf("installed account returned %d picker models", len(models))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir, nonce, ref := t.TempDir(), id.New("sessioncheck"), ""
	schema := json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"],"additionalProperties":false}`)
	spec := model.RunSpec{ReadOnly: true, Instructions: "This is a tiny integration check. Do not use any tools or read files. Return only the requested JSON.", TaskGoal: "Remember this marker for the next turn: " + nonce + ". Return {\"message\":\"ready\"}.", OutputSchema: schema}
	result := a.Run(ctx, spec, dir, nil, func(e Event) {
		if e.Type == "session.bound" {
			ref = e.AgentSessionRef
		}
	})
	if result.Err != nil || ref == "" {
		t.Fatal("initial Codex turn/session failed", result.Err)
	}
	var answer struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(result.Output), &answer) != nil || answer.Message != "ready" {
		t.Fatal("initial structured output failed")
	}
	spec.AgentSessionRef, spec.RequireNativeSession = ref, true
	spec.TaskGoal = "Return the marker from the previous turn as the message property. Do not invent a new marker."
	result = a.Run(ctx, spec, dir, nil, func(Event) {})
	if result.Err != nil || json.Unmarshal([]byte(result.Output), &answer) != nil || answer.Message != nonce {
		t.Fatal("resumed native session did not retain context", result.Err)
	}
	t.Log("real structured output and original native-session context verified")
}
