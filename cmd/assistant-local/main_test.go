package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func TestLocalAdapterSelection(t *testing.T) {
	binary, err := exec.LookPath("true")
	if err != nil {
		t.Skip("needs true executable")
	}
	for _, name := range []string{"codex-agent", "cursor-agent"} {
		a, err := localAdapter(name, binary, binary)
		if err != nil || a.Name() != name {
			t.Fatal(a, err)
		}
	}
	if _, err = localAdapter("typo", binary, binary); err == nil {
		t.Fatal("unknown adapter fell back silently")
	}
}
func TestBootstrapCreatesCursorAndPreservesEditedMemberOnRestart(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RegisterRuntime(context.Background(), model.RuntimeHello{RuntimeID: "dev-cursor", Epoch: "boot", Capabilities: map[string]any{"adapters": map[string]any{"cursor-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}}}}); err != nil {
		t.Fatal(err)
	}
	boot := func(adapter string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
		defer cancel()
		if err := bootstrap(ctx, s, "dev-cursor", "auto", "127.0.0.1:17343", adapter); err != nil {
			t.Fatal(err)
		}
	}
	boot("cursor-agent")
	agents, err := s.ListAgents(context.Background())
	if err != nil || len(agents) != 1 || agents[0].AdapterID != "cursor-agent" {
		t.Fatal(agents, err)
	}
	a := agents[0]
	if _, err = s.UpdateAgent(context.Background(), a.ID, store.AgentUpdate{Name: "用户自定成员", ModelID: "changed-model", MaxConcurrent: 1, State: "ACTIVE", ExpectedVersion: a.Version}); err != nil {
		t.Fatal(err)
	}
	boot("codex-agent")
	agents, err = s.ListAgents(context.Background())
	if err != nil || len(agents) != 1 || agents[0].Name != "用户自定成员" || agents[0].ModelID != "changed-model" || agents[0].AdapterID != "cursor-agent" {
		t.Fatal("bootstrap rewrote existing member", agents, err)
	}
}
