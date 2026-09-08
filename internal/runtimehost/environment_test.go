package runtimehost

import (
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
	for _, invalid := range []string{"partial", "WORK_ASSISTANT_RESULT={}", "WORK_ASSISTANT_RESULT=not json"} {
		if _, err = decodeWindowsResult(invalid); err == nil {
			t.Fatal("invalid result accepted")
		}
	}
}

func TestWindowsBuildUsesProductBaselineAndGuardsRetries(t *testing.T) {
	for _, required := range []string{"product_compile_commands", "Probe compiler differs from product build", "PRODUCT_CXX_FLAGS=", "-DCMAKE_CXX_FLAGS=", "CMakeConfigureLog.yaml", "BUILD_INPUT_SHA256=", "$file.name -ne 'README.md'"} {
		if !strings.Contains(windowsPhase0Script, required) {
			t.Fatal("missing build safety", required)
		}
	}
}
