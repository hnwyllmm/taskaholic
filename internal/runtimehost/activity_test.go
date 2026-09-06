package runtimehost

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
)

func TestActionOutboxDurableAndRedactedBeforeReplay(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	s, err := OpenSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"RUNNING", "COMPLETED"} {
		_, err = s.EnqueueEvent(ctx, model.RuntimeEvent{RuntimeID: "machine", Epoch: "old-epoch", RunID: "run", Type: "run.progress", Message: "password=never-persist-this", Activity: &model.Action{ID: "scope/item", Kind: "command", State: state, Command: "go test ./...", Output: "api_key=never-persist-this"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, want := range []string{"RUNNING", "COMPLETED"} {
		out, err := s.NextOutbound(ctx)
		if err != nil || out == nil {
			t.Fatal(out, err)
		}
		if strings.Contains(string(out.Params), "never-persist-this") {
			t.Fatal("secret reached durable spool")
		}
		var got model.RuntimeEvent
		if err = json.Unmarshal(out.Params, &got); err != nil {
			t.Fatal(err)
		}
		if got.RuntimeSeq != int64(i+1) || got.Epoch != "old-epoch" || got.Activity == nil || got.Activity.State != want || got.Activity.ID != "scope/item" || !got.Activity.Redacted {
			t.Fatal(got)
		}
		if err = s.MarkOutboundDelivered(ctx, out.RuntimeSeq); err != nil {
			t.Fatal(err)
		}
	}
}
