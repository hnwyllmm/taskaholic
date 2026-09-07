package agent

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"work-assistant/internal/model"
)

func TestReasoningCatalogUsesActualOptions(t *testing.T) {
	var input bytes.Buffer
	models, err := readCodexModels(&input, strings.NewReader(`{"id":1,"result":{}}
{"id":2,"result":{"data":[{"model":"model-a","defaultReasoningEffort":"low","supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Light"},{"reasoningEffort":"ultra"},{"reasoningEffort":"ultra"},{"reasoningEffort":"bad\nvalue"}]},{"model":"model-b"}]}}
`))
	if err != nil || len(models) != 2 || len(models[0].ReasoningEfforts) != 2 || models[0].DefaultReasoningEffort != "low" || len(models[1].ReasoningEfforts) != 0 {
		t.Fatal(models, err)
	}
	if actual, err := model.ResolveReasoningEffort(models, "model-a", "ultra"); err != nil || actual != "model-a" {
		t.Fatal(actual, err)
	}
	for _, pair := range [][2]string{{"model-a", "high"}, {"model-b", "low"}, {"custom", "high"}, {"", "high"}, {"model-a", "high\""}} {
		if _, err := model.ResolveReasoningEffort(models, pair[0], pair[1]); err == nil {
			t.Fatal("unsupported option accepted", pair)
		}
	}
	if actual, err := model.ResolveReasoningEffort(models, "custom[effort=high]", ""); err != nil || actual != "custom[effort=high]" {
		t.Fatal(actual, err)
	}
}

func TestCursorEffortVariantsPreserveSpeedAndNeverInventModels(t *testing.T) {
	models, err := parseCursorModels("auto - Auto\nexample-low - Low\nexample-high - High\nexample-low-fast - Low Fast\nexample-high-fast - High Fast\nother-high - Other\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(models[0].ReasoningEfforts) != 0 || len(models[5].ReasoningEfforts) != 0 {
		t.Fatal("invented effort options", models)
	}
	for _, pair := range [][2]string{{"example-low", "example-high"}, {"example-low-fast", "example-high-fast"}} {
		actual, err := model.ResolveReasoningEffort(models, pair[0], "high")
		if err != nil || actual != pair[1] {
			t.Fatal(actual, err)
		}
	}
	if _, err := model.ResolveReasoningEffort(models, "example-low", "ultra"); err == nil {
		t.Fatal("invented variant")
	}
}

const reasoningCodexCatalog = `if [ "$1" = app-server ]; then
read init
printf '%s\n' '{"id":1,"result":{}}'
read initialized
read models
printf '%s\n' '{"id":2,"result":{"data":[{"model":"test-model","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"high"}]}]}}'
exit 0
fi
`

func TestCodexEffortArgumentsForNewResumeAndDirective(t *testing.T) {
	fake, dir, log := fakeCursor(t, reasoningCodexCatalog+`printf '%s\n' --call-- "$@" >> "$CURSOR_TEST_ARGS"
printf '%s\n' '{"type":"thread.started","thread_id":"original"}' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}' '{"type":"turn.completed"}'
`)
	a, err := NewCodexAdapter(fake.binary, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	spec := model.RunSpec{ExecutionSettings: model.ExecutionSettings{ReasoningEffort: "high"}, ModelID: "test-model", TaskGoal: "small check", ReadOnly: true}
	directives := make(chan model.Directive, 1)
	directives <- model.Directive{ID: "guide", Kind: model.DirectiveKindMessage, Message: "follow up"}
	configured := 0
	emit := func(e Event) {
		if e.Execution != nil {
			configured++
			if e.Execution.ReasoningEffort != "high" || e.Execution.ExecutionModelID != "test-model" {
				t.Error(e.Execution)
			}
		}
	}
	if result := a.Run(context.Background(), spec, dir, directives, emit); result.Err != nil {
		t.Fatal(result.Err)
	}
	spec.AgentSessionRef, spec.RequireNativeSession = "codex:original", true
	if result := a.Run(context.Background(), spec, dir, nil, emit); result.Err != nil {
		t.Fatal(result.Err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `model_reasoning_effort="high"`) != 3 || strings.Count(string(raw), "resume\n") != 2 || configured != 2 {
		t.Fatal("effort/session not repeated", string(raw), configured)
	}
	if strings.Contains(string(raw), "danger-full-access") {
		t.Fatal("permissions changed")
	}
	spec.ReasoningEffort = "unsupported"
	if result := a.Run(context.Background(), spec, dir, nil, emit); result.Err == nil {
		t.Fatal("silently fell back")
	}
	after, _ := os.ReadFile(log)
	if !bytes.Equal(raw, after) {
		t.Fatal("invalid configuration started a task")
	}
}

func TestCursorEffortIsAnExactDiscoveredVariantOnOriginalSession(t *testing.T) {
	a, dir, log := fakeCursor(t, `if [ "$1" = models ]; then
printf '%s\n' 'family-low-fast - Low Fast' 'family-high-fast - High Fast'
exit 0
fi
printf '%s\n' "$@" >> "$CURSOR_TEST_ARGS"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"original"}' '{"type":"result","subtype":"success","is_error":false,"session_id":"original","result":"ok"}'
`)
	spec := model.RunSpec{ExecutionSettings: model.ExecutionSettings{ReasoningEffort: "high"}, ModelID: "family-low-fast", AgentSessionRef: "cursor:original", RequireNativeSession: true, TaskGoal: "check"}
	var config *model.ExecutionSettings
	result := a.Run(context.Background(), spec, dir, nil, func(e Event) {
		if e.Execution != nil {
			config = e.Execution
		}
	})
	if result.Err != nil || config == nil || config.ExecutionModelID != "family-high-fast" {
		t.Fatal(result, config)
	}
	raw, _ := os.ReadFile(log)
	if !strings.Contains(string(raw), "--model\nfamily-high-fast\n") || !strings.Contains(string(raw), "--resume\noriginal\n") || strings.Contains(string(raw), "--force") {
		t.Fatal(string(raw))
	}
}
