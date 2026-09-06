//go:build !linux && !darwin

package agent

import "os/exec"

func configureCursorProcess(command *exec.Cmd) {}
