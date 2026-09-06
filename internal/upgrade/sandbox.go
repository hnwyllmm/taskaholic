package upgrade

import (
	"fmt"
	"path/filepath"
	"runtime"
)

// ValidationSandbox is the plug-in boundary used for every command that loads
// candidate code. Implementations must fail closed unless they can prevent
// network access and writes outside jobDir.
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

func defaultValidationSandbox() ValidationSandbox {
	if runtime.GOOS == "darwin" {
		return seatbeltValidationSandbox{binary: "/usr/bin/sandbox-exec"}
	}
	return unavailableValidationSandbox{reason: "this release requires a ValidationSandbox plug-in outside macOS"}
}
