package runtimehost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
)

func TestCappedWriterDrainsWithoutGrowingPastLimit(t *testing.T) {
	var destination bytes.Buffer
	writer := newCappedWriter(&destination, 5, "stdout")
	for _, chunk := range [][]byte{[]byte("abc"), []byte("defgh"), []byte("ignored")} {
		if n, err := writer.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if !writer.truncated {
		t.Fatal("oversized output was not marked truncated")
	}
	if got := destination.String(); !strings.HasPrefix(got, "abcde\n[work-assistant:") || strings.Contains(got, "fgh") || strings.Contains(got, "ignored") {
		t.Fatalf("unexpected capped output: %q", got)
	}
}

func TestVMOperationCancellationLetsTransportCleanUp(t *testing.T) {
	dir := t.TempDir()
	workspace, _ := os.OpenRoot(dir)
	defer workspace.Close()
	mailboxDir := t.TempDir()
	mailbox, _ := os.OpenRoot(mailboxDir)
	defer mailbox.Close()
	transport := filepath.Join(t.TempDir(), "transport")
	script := "#!/bin/sh\n" +
		"trap 'echo interrupted > \"$0.interrupted\"; exit 130' INT\n" +
		"cat >/dev/null\n" +
		"touch \"$0.started\"\n" +
		"while :; do sleep 0.02; done\n"
	if err := os.WriteFile(transport, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	opCtx, cancel := context.WithCancel(context.Background())
	done := make(chan vmResponse, 1)
	go func() {
		d := &Daemon{}
		done <- d.executeVMOperation(opCtx, transport, vmRequest{Operation: "exec", Script: "Write-Output hello"}, workspace, mailbox, t.TempDir(), "cancel")
	}()
	waitForFile(t, transport+".started", 3*time.Second)
	cancel()
	select {
	case result := <-done:
		if result.ExitCode == 0 {
			t.Fatal("cancelled transport reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled transport did not stop")
	}
	waitForFile(t, transport+".interrupted", 3*time.Second)
}

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
	waitForMissingFile(t, filepath.Join(session, ".assistant-vm-run_test", "active"), 3*time.Second)
	spec.RunID = "run_readonly"
	spec.ReadOnly = true
	instructions, stop, err = d.startVMBridge(context.Background(), spec, session, source, emit)
	if err != nil || instructions != "" {
		t.Fatal("readonly bridge enabled", err)
	}
	stop()
}

func TestVMBridgeDrainsSubmittedOperationsAfterNormalTurnEnd(t *testing.T) {
	root := t.TempDir()
	session := t.TempDir()
	source, _ := filepath.EvalSymlinks(t.TempDir())
	transport := filepath.Join(t.TempDir(), "transport")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"touch \"$0.started\"\n" +
		"while [ ! -f \"$0.release\" ]; do sleep 0.02; done\n"
	if err := os.WriteFile(transport, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: Config{WorkRoot: root}}
	raw, _ := json.Marshal(map[string]windowsProfile{"windows_seekdb_phase0": {Autonomous: true, WinRMCommand: transport}})
	if err := os.WriteFile(filepath.Join(root, "windows-profiles.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	spec := model.RunSpec{RunID: "run_drain", ExecutionGrant: &model.ExecutionGrant{ReviewID: "approved"}}
	_, stop, err := d.startVMBridge(context.Background(), spec, session, source, func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	mailbox := filepath.Join(session, ".assistant-vm-run_drain")
	request, _ := json.Marshal(vmRequest{Operation: "exec", Script: "Write-Output hello"})
	firstID := strings.Repeat("a", 32)
	secondID := strings.Repeat("b", 32)
	if err = os.WriteFile(filepath.Join(mailbox, firstID+".request"), request, 0600); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, transport+".started", 3*time.Second)
	if err = os.WriteFile(filepath.Join(mailbox, secondID+".request"), request, 0600); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	stop()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("normal stop waited for VM operation: %s", elapsed)
	}
	if _, err = os.Stat(filepath.Join(mailbox, "active")); err != nil {
		t.Fatalf("bridge stopped before submitted operations drained: %v", err)
	}
	if err = os.WriteFile(transport+".release", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{firstID, secondID} {
		path := filepath.Join(mailbox, id+".response")
		waitForFile(t, path, 3*time.Second)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var response vmResponse
		if json.Unmarshal(data, &response) != nil || response.ExitCode != 0 {
			t.Fatalf("%s did not complete: %s", id, data)
		}
	}
	waitForMissingFile(t, filepath.Join(mailbox, "active"), 3*time.Second)
}

func TestVMBridgeRetrievesPersistedResultAcrossRunsWithoutUsingWindowsGate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	session := t.TempDir()
	source, _ := filepath.EvalSymlinks(t.TempDir())
	transport := filepath.Join(t.TempDir(), "transport")
	marker := transport + ".called"
	if err := os.WriteFile(transport, []byte("#!/bin/sh\ntouch \""+marker+"\"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	spool, err := OpenSpool(filepath.Join(root, "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	accept := func(message string, spec model.RunSpec) {
		raw, _ := json.Marshal(spec)
		if _, acceptErr := spool.AcceptRunStart(ctx, message, raw, spec, session); acceptErr != nil {
			t.Fatal(acceptErr)
		}
	}
	grant := &model.ExecutionGrant{ReviewID: "approved"}
	prior := model.RunSpec{RunID: "run_prior", TaskID: "task_one", SessionID: "session_one", ExecutionGrant: grant}
	current := model.RunSpec{RunID: "run_current", TaskID: prior.TaskID, SessionID: prior.SessionID, ExecutionGrant: grant}
	accept("message_prior", prior)
	accept("message_current", current)
	operationID := strings.Repeat("c", 32)
	record := filepath.Join(root, "vm-operations", prior.RunID, operationID)
	if err = os.MkdirAll(record, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(record, "stdout.log"), []byte("persisted stdout\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(record, "stderr.log"), []byte("persisted stderr\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(vmResponse{ExitCode: 7, Error: "exit status 7"})
	if err = os.WriteFile(filepath.Join(record, "result.json"), result, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]windowsProfile{"windows_seekdb_phase0": {Autonomous: true, WinRMCommand: transport}})
	if err = os.WriteFile(filepath.Join(root, "windows-profiles.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: Config{WorkRoot: root}, spool: spool, windowsGate: make(chan struct{}, 1)}
	d.windowsGate <- struct{}{} // A real Windows operation may be occupying the VM.
	_, stop, err := d.startVMBridge(ctx, current, session, source, func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client := filepath.Join(session, ".assistant-vm-"+current.RunID, "client.py")
	started := time.Now()
	cmd := exec.Command("python3", client, "result", operationID)
	output, runErr := cmd.CombinedOutput()
	if runErr == nil || cmd.ProcessState.ExitCode() != 7 {
		t.Fatalf("result lookup exit = %v, output %s", runErr, output)
	}
	if time.Since(started) > 2*time.Second || !strings.Contains(string(output), "persisted stdout") || !strings.Contains(string(output), "persisted stderr") {
		t.Fatalf("result lookup waited for VM gate or lost output: %s", output)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("result lookup invoked Windows transport")
	}
}

func TestVMBridgeRejectsPersistedResultFromAnotherSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	session := t.TempDir()
	source, _ := filepath.EvalSymlinks(t.TempDir())
	transport := filepath.Join(t.TempDir(), "transport")
	if err := os.WriteFile(transport, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	spool, err := OpenSpool(filepath.Join(root, "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	grant := &model.ExecutionGrant{ReviewID: "approved"}
	prior := model.RunSpec{RunID: "run_other", TaskID: "task_one", SessionID: "session_other", ExecutionGrant: grant}
	current := model.RunSpec{RunID: "run_current", TaskID: prior.TaskID, SessionID: "session_current", ExecutionGrant: grant}
	for index, spec := range []model.RunSpec{prior, current} {
		raw, _ := json.Marshal(spec)
		if _, err = spool.AcceptRunStart(ctx, fmt.Sprintf("message_%d", index), raw, spec, session); err != nil {
			t.Fatal(err)
		}
	}
	operationID := strings.Repeat("d", 32)
	record := filepath.Join(root, "vm-operations", prior.RunID, operationID)
	if err = os.MkdirAll(record, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(record, "stdout.log"), []byte("secret output"), 0600); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(vmResponse{ExitCode: 0})
	if err = os.WriteFile(filepath.Join(record, "result.json"), result, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]windowsProfile{"windows_seekdb_phase0": {Autonomous: true, WinRMCommand: transport}})
	if err = os.WriteFile(filepath.Join(root, "windows-profiles.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: Config{WorkRoot: root}, spool: spool}
	_, stop, err := d.startVMBridge(ctx, current, session, source, func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client := filepath.Join(session, ".assistant-vm-"+current.RunID, "client.py")
	cmd := exec.Command("python3", client, "result", operationID)
	output, runErr := cmd.CombinedOutput()
	if runErr == nil || strings.Contains(string(output), "secret output") || !strings.Contains(string(output), "not available") {
		t.Fatalf("cross-session result was exposed: %v, %s", runErr, output)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForMissingFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to be removed", path)
}
