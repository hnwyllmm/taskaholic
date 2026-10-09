package runtimehost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsSnapshotFixedFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range phase0Files {
		if err = os.WriteFile(filepath.Join(real, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, hash, err := snapshotWindowsFiles(real)
	if err != nil || len(files) != 6 || len(hash) != 64 {
		t.Fatal(err, hash)
	}
	_, again, _ := snapshotWindowsFiles(real)
	if hash != again {
		t.Fatal("unstable snapshot")
	}
	os.WriteFile(filepath.Join(real, "README.md"), []byte("changed"), 0600)
	_, changed, _ := snapshotWindowsFiles(real)
	if changed == hash {
		t.Fatal("snapshot does not cover all files")
	}
	os.Remove(filepath.Join(real, "README.md"))
	os.Symlink(filepath.Join(real, "CMakeLists.txt"), filepath.Join(real, "README.md"))
	if _, _, err = snapshotWindowsFiles(real); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestWindowsLogRetainsFinalVerdict(t *testing.T) {
	var log boundedWindowsLog
	log.Write([]byte(strings.Repeat("x", 200000)))
	log.Write([]byte("\nWORK_ASSISTANT_RESULT={\"status\":\"failed\",\"message\":\"compile failed\"}\r\n"))
	result, err := decodeWindowsResult(log.String())
	if err != nil || result.Status != "failed" || log.Len() > 96*1024 {
		t.Fatal(err)
	}
	withStderr, err := decodeWindowsResult(log.String() + "late-native-stderr\n")
	if err != nil || !strings.Contains(withStderr.Log, "late-native-stderr") {
		t.Fatal("lost stderr after envelope", err)
	}
	for _, invalid := range []string{"partial", "WORK_ASSISTANT_RESULT={}", "WORK_ASSISTANT_RESULT=not json"} {
		if _, err = decodeWindowsResult(invalid); err == nil {
			t.Fatal("invalid result accepted")
		}
	}
}

func TestWindowsBuildUsesCurrentRepositoryWithoutApprovalHash(t *testing.T) {
	for _, required := range []string{"repository_sha256", "'phase0','-j','4'", "Repository transfer checksum mismatch", "Probe changed during repository snapshot", "-RedirectStandardError $buildErr", "-RedirectStandardError $probeErr", "BUILD_EXIT_CODE=", "PROBE_EXIT_CODE"} {
		if !strings.Contains(windowsPhase0Script, required) {
			t.Fatal("missing build safety", required)
		}
	}
	for _, forbidden := range []string{"& $cfg.cmake", "& $cfg.clang", "& $cfg.ninja", "CMAKE_CXX_FLAGS", "product_compile_commands", "_ALLOW_COMPILER_AND_STL_VERSION_MISMATCH", "$cfg.build_script_sha256", "build-result.json", "WorkAssistantRequest"} {
		if strings.Contains(windowsPhase0Script, forbidden) {
			t.Fatal("executor still owns build parameters", forbidden)
		}
	}
}

func TestAutonomousWindowsEnvironmentPreflight(t *testing.T) {
	root := t.TempDir()
	transport := filepath.Join(t.TempDir(), "winrm-transport")
	if err := os.WriteFile(transport, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'WORK_ASSISTANT_VM_READY\\nWORK_ASSISTANT_FREE_GB=42\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	profiles, err := json.Marshal(map[string]windowsProfile{"windows_seekdb_phase0": {
		Autonomous: true, WinRMCommand: transport, MinimumFreeGB: 12,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "windows-profiles.json"), profiles, 0600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: Config{WorkRoot: root}}
	capability := d.executionCapabilities()["windows_vm"]
	if !capability.Available || capability.CheckedAtMS == 0 || capability.Fingerprint == "" {
		t.Fatalf("unexpected healthy capability: %#v", capability)
	}
	// A second hello must use the cached read-only result, rather than probe a
	// working VM for every Manager connection.
	if repeated := d.executionCapabilities()["windows_vm"]; repeated != capability {
		t.Fatalf("preflight was not stable: %#v != %#v", repeated, capability)
	}
}

func TestAutonomousWindowsEnvironmentPreflightRejectsLowDisk(t *testing.T) {
	root := t.TempDir()
	transport := filepath.Join(t.TempDir(), "winrm-transport")
	if err := os.WriteFile(transport, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'WORK_ASSISTANT_VM_READY\\nWORK_ASSISTANT_FREE_GB=4\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	profiles, err := json.Marshal(map[string]windowsProfile{"windows_seekdb_phase0": {
		Autonomous: true, WinRMCommand: transport, MinimumFreeGB: 12,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "windows-profiles.json"), profiles, 0600); err != nil {
		t.Fatal(err)
	}
	capability := (&Daemon{config: Config{WorkRoot: root}}).executionCapabilities()["windows_vm"]
	if capability.Available || !strings.Contains(capability.Reason, "4 GiB") {
		t.Fatalf("low disk was accepted: %#v", capability)
	}
}
