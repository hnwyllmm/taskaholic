package agent

import (
	"context"
	"runtime"
	"sync"
	"testing"

	"work-assistant/internal/model"
)

func TestExecAdapterStreamsBothOutputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}
	var mutex sync.Mutex
	var events []Event
	result := (ExecAdapter{}).Run(context.Background(), model.RunSpec{
		Command: []string{"/bin/sh", "-c", "echo from-stdout; echo from-stderr >&2"},
	}, t.TempDir(), nil, func(event Event) {
		mutex.Lock()
		events = append(events, event)
		mutex.Unlock()
	})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	mutex.Lock()
	defer mutex.Unlock()
	found := map[string]string{}
	for _, event := range events {
		if event.Stream == "runtime" {
			t.Fatalf("unexpected runtime diagnostic: %#v", event)
		}
		found[event.Stream] = event.Message
	}
	if found["stdout"] != "from-stdout" || found["stderr"] != "from-stderr" {
		t.Fatalf("events = %#v", events)
	}
}
