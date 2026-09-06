package router

import (
	"context"
	"encoding/json"
	"testing"

	"work-assistant/internal/model"
)

func TestFirstOnlineSelectsDeterministicallyAndChecksAdapter(t *testing.T) {
	capable, _ := json.Marshal(map[string]any{"adapters": map[string]any{"exec-agent": map[string]any{}}})
	incapable, _ := json.Marshal(map[string]any{"adapters": map[string]any{"other": map[string]any{}}})
	runtimes := []model.Runtime{
		{ID: "runtime-z", State: "ONLINE", Capabilities: capable},
		{ID: "runtime-a", State: "OFFLINE", Capabilities: capable},
		{ID: "runtime-b", State: "ONLINE", Capabilities: incapable},
		{ID: "runtime-c", State: "ONLINE", Capabilities: capable},
	}
	selected, err := (FirstOnline{}).Select(context.Background(), Request{AdapterID: "exec-agent"}, runtimes)
	if err != nil || selected != "runtime-c" {
		t.Fatalf("selected %q, error %v", selected, err)
	}
}
