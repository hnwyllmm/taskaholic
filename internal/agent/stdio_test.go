package agent

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func TestStdioAdapterAppliesLiveDirective(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}
	directives := make(chan model.Directive, 1)
	directives <- model.Directive{ID: "directive-1", Kind: model.DirectiveKindMessage, Message: "new direction"}
	var mutex sync.Mutex
	var events []Event
	result := (StdioAdapter{}).Run(context.Background(), model.RunSpec{
		Command: []string{"/bin/sh", "-c", "IFS= read -r line; echo \"$line\""},
	}, t.TempDir(), directives, func(event Event) {
		mutex.Lock()
		events = append(events, event)
		mutex.Unlock()
	})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mutex.Lock()
		foundApplied, foundWire := false, false
		for _, event := range events {
			foundApplied = foundApplied || (event.Type == "directive.applied" && event.DirectiveID == "directive-1")
			foundWire = foundWire || (event.Stream == "stdout" && event.Message != "")
		}
		mutex.Unlock()
		if foundApplied && foundWire {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("events = %#v", events)
}
