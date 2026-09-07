package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/runtimehost"
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

func TestAdditionalAdaptersKeepPrimaryAndRejectUnavailable(t *testing.T) {
	binary, err := exec.LookPath("true")
	if err != nil {
		t.Skip("needs true executable")
	}
	adapters, err := localAdapters("cursor-agent", " codex-agent,cursor-agent,codex-agent ", binary, binary)
	if err != nil || len(adapters) != 2 || adapters[0].Name() != "cursor-agent" || adapters[1].Name() != "codex-agent" {
		t.Fatal(adapters, err)
	}
	if adapters[1].Capabilities()["sandbox"] != "read-only" {
		t.Fatal("additional adapter widened execution permissions")
	}
	for _, extra := range []string{"typo", "codex-agent,"} {
		if _, err := localAdapters("cursor-agent", extra, binary, binary); err == nil {
			t.Fatal("invalid explicit adapter was ignored", extra)
		}
	}
	if _, err := localAdapters("cursor-agent", "codex-agent", filepath.Join(t.TempDir(), "missing-codex"), binary); err == nil {
		t.Fatal("unavailable explicit adapter was ignored")
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

// Exercise the actual offline rename module against a real store, then restart
// the real bootstrap. The runtime-specific idempotency key must still resolve
// the original helper, otherwise startup tries to create a duplicate member.
func TestBootstrapAfterOfflineRuntimeRename(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("runtime rename tooling requires Node with node:sqlite")
	}
	dir := t.TempDir()
	database := filepath.Join(dir, "control.sqlite")
	s, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	ctx := context.Background()
	register := func(runtimeID string) {
		t.Helper()
		if err := s.RegisterRuntime(ctx, model.RuntimeHello{RuntimeID: runtimeID, Epoch: "boot", Capabilities: map[string]any{"adapters": map[string]any{"cursor-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}}}}); err != nil {
			t.Fatal(err)
		}
	}
	boot := func(runtimeID string) {
		t.Helper()
		bootCtx, cancel := context.WithTimeout(ctx, 450*time.Millisecond)
		defer cancel()
		if err := bootstrap(bootCtx, s, runtimeID, "auto", "127.0.0.1:17343", "cursor-agent"); err != nil {
			t.Fatal(err)
		}
	}
	register("dev-cursor")
	boot("dev-cursor")
	before, err := s.ListAgents(ctx)
	if err != nil || len(before) != 1 {
		t.Fatal("initial bootstrap", err)
	}
	drafts, err := s.ListRoleDrafts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAllRuntimesOffline(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	spoolPath := filepath.Join(dir, "runtime.sqlite")
	spool, err := runtimehost.OpenSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	spool.Close()
	module, err := filepath.Abs("../../deploy/dev/rename-runtime.mjs")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "--input-type=module", "-e", `import {DatabaseSync} from 'node:sqlite'; const {renameRuntime}=await import(process.argv[1]); const c=new DatabaseSync(process.argv[2]),s=new DatabaseSync(process.argv[3]); try{renameRuntime(c,s,'dev-cursor','dev')}finally{c.close();s.close()}`, module, database, spoolPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("offline rename: %v: %s", err, output)
	}
	s, err = store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	register("dev")
	boot("dev")
	after, err := s.ListAgents(ctx)
	if err != nil || len(after) != 1 || after[0].ID != before[0].ID || after[0].RuntimeID != "dev" || after[0].RoleID != before[0].RoleID {
		t.Fatal("renamed bootstrap replaced helper", err)
	}
	afterDrafts, err := s.ListRoleDrafts(ctx)
	if err != nil || len(afterDrafts) != len(drafts) || afterDrafts[0].ID != drafts[0].ID {
		t.Fatal("renamed bootstrap created another role draft", err)
	}
}
