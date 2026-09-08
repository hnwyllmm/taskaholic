package runtimehost

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
)

func TestVMOperationsStayInsideLocalWorkspace(t *testing.T) {
	dir := t.TempDir()
	workspace, _ := os.OpenRoot(dir)
	defer workspace.Close()
	mailboxDir := t.TempDir()
	mailbox, _ := os.OpenRoot(mailboxDir)
	defer mailbox.Close()
	transport := filepath.Join(t.TempDir(), "transport")
	if err := os.WriteFile(transport, []byte("#!/bin/sh\nif [ \"$1\" = --upload ]; then cat \"$2\"; else cat; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{}
	execute := func(id string, r vmRequest) vmResponse {
		t.Helper()
		return d.executeVMOperation(context.Background(), transport, r, workspace, mailbox, t.TempDir(), id)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("test payload"), 0600); err != nil {
		t.Fatal(err)
	}
	result := execute("upload", vmRequest{Operation: "upload", Source: "payload", Destination: "C:\\test.bin"})
	if result.ExitCode != 0 {
		t.Fatal(result)
	}
	data, err := os.ReadFile(filepath.Join(mailboxDir, "upload.stdout"))
	if err != nil || string(data) != "test payload" {
		t.Fatal(err, string(data))
	}
	for _, source := range []string{"../outside", "/etc/passwd", ".git/config"} {
		if execute("bad", vmRequest{Operation: "upload", Source: source, Destination: "C:\\test.bin"}).ExitCode == 0 {
			t.Fatal("host path accepted", source)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("not allowed"), 0600)
	os.Symlink(outside, filepath.Join(dir, "escape"))
	if execute("symlink", vmRequest{Operation: "upload", Source: "escape", Destination: "C:\\test.bin"}).ExitCode == 0 {
		t.Fatal("symlink escape")
	}
	if execute("hostexec", vmRequest{Operation: "host_shell", Script: "something"}).ExitCode == 0 {
		t.Fatal("host operation accepted")
	}
	if execute("exec", vmRequest{Operation: "exec", Script: "Get-Date"}).ExitCode != 0 {
		t.Fatal("VM exec rejected")
	}
}

func TestVMBridgeIsRunScopedAndOptIn(t *testing.T) {
	root := t.TempDir()
	session := t.TempDir()
	source, _ := filepath.EvalSymlinks(t.TempDir())
	transport := filepath.Join(t.TempDir(), "transport")
	os.WriteFile(transport, []byte("#!/bin/sh\ncat\nexit 7\n"), 0700)
	d := &Daemon{config: Config{WorkRoot: root}}
	spec := model.RunSpec{RunID: "run_test"}
	emit := func(agent.Event) {}
	instructions, stop, err := d.startVMBridge(context.Background(), spec, session, source, emit)
	if err != nil || instructions != "" {
		t.Fatal("unapproved enabled", err)
	}
	stop()
	raw, _ := json.Marshal(map[string]windowsProfile{"windows_seekdb_phase0": {Autonomous: true, WinRMCommand: transport}})
	os.WriteFile(filepath.Join(root, "windows-profiles.json"), raw, 0600)
	spec.ExecutionGrant = &model.ExecutionGrant{ReviewID: "approved"}
	instructions, stop, err = d.startVMBridge(context.Background(), spec, session, source, emit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions, "exec --script") {
		t.Fatal(instructions)
	}
	client := filepath.Join(session, ".assistant-vm-run_test", "client.py")
	cmd := exec.Command("python3", client, "exec", "--script", "Write-Output hello")
	out, err := cmd.CombinedOutput()
	if err == nil || cmd.ProcessState.ExitCode() != 7 || !strings.Contains(string(out), "Write-Output hello") {
		t.Fatal("lost native result", err, string(out))
	}
	stop()
	if _, err = os.Stat(filepath.Join(session, ".assistant-vm-run_test", "active")); !os.IsNotExist(err) {
		t.Fatal("bridge still active")
	}
	spec.RunID = "run_readonly"
	spec.ReadOnly = true
	instructions, stop, err = d.startVMBridge(context.Background(), spec, session, source, emit)
	if err != nil || instructions != "" {
		t.Fatal("readonly bridge enabled", err)
	}
	stop()
}
