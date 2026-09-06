package upgrade

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ValidationSandbox is the plug-in boundary used for every command that loads
// candidate code. Implementations must fail closed unless they can prevent
// host/external network access and writes outside jobDir. Linux permits private
// loopback for httptest servers, but never shares the host network namespace.
type ValidationSandbox interface {
	Name() string
	Wrap(jobDir string, argv []string) ([]string, error)
}

type seatbeltValidationSandbox struct {
	binary string
}

func (seatbeltValidationSandbox) Name() string { return "macOS Seatbelt" }

func (s seatbeltValidationSandbox) Wrap(jobDir string, argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("validation command is empty")
	}
	resolved, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		return nil, fmt.Errorf("resolve upgrade job directory: %w", err)
	}
	profile := `(version 1)
(allow default)
(deny network*)
(deny file-write*
  (require-all
    (require-not (subpath (param "UPGRADE_JOB_DIR")))
    (require-not (literal "/dev/null"))))
(deny file-write-unlink (literal (param "UPGRADE_JOB_DIR")))
(deny signal (require-not (target same-sandbox)))`
	wrapped := []string{s.binary, "-D", "UPGRADE_JOB_DIR=" + resolved, "-p", profile}
	return append(wrapped, argv...), nil
}

type unavailableValidationSandbox struct {
	reason string
}

func (unavailableValidationSandbox) Name() string { return "unavailable" }

func (s unavailableValidationSandbox) Wrap(_ string, _ []string) ([]string, error) {
	return nil, fmt.Errorf("candidate validation sandbox unavailable: %s", s.reason)
}

func defaultValidationSandbox(readOnlyPaths ...string) ValidationSandbox {
	if runtime.GOOS == "darwin" {
		return seatbeltValidationSandbox{binary: "/usr/bin/sandbox-exec"}
	}
	if runtime.GOOS == "linux" {
		binary, err := exec.LookPath("bwrap")
		if err == nil {
			return bubblewrapValidationSandbox{binary: binary, readOnlyPaths: readOnlyPaths}
		}
		return unavailableValidationSandbox{reason: "Linux requires bubblewrap (bwrap) with working user namespaces"}
	}
	return unavailableValidationSandbox{reason: "this platform requires a ValidationSandbox plug-in"}
}

type bubblewrapValidationSandbox struct {
	binary        string
	readOnlyPaths []string
}

func (bubblewrapValidationSandbox) Name() string { return "Linux bubblewrap" }

func (s bubblewrapValidationSandbox) Wrap(jobDir string, argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("validation command is empty")
	}
	job, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		return nil, err
	}
	job, err = filepath.Abs(job)
	if err != nil || job == "/" {
		return nil, fmt.Errorf("invalid upgrade job directory")
	}
	args := []string{s.binary, "--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL"}
	// Start with an empty filesystem, not a read-only view of the host's home,
	// credentials and live databases. Mount only system tools and pinned caches.
	paths := append([]string{"/usr", "/bin", "/sbin", "/lib", "/lib64"}, s.readOnlyPaths...)
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		if path == "/" || path == job || strings.HasPrefix(job, path+"/") {
			return nil, fmt.Errorf("sandbox read-only mount overlaps upgrade data: %s", path)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		args = append(args, "--ro-bind", path, path)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--dir", "/tmp", "--bind", job, job, "--remount-ro", "/", "--")
	return append(args, argv...), nil
}
