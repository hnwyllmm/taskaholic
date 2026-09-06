package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"work-assistant/internal/model"
)

// Discover using the installed CLI/account, rather than shipping a stale list.
// This command does not start an agent task or open/resume a chat.
func (a *CursorAdapter) ListModels(parent context.Context) ([]model.ModelOption, error) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.binary, "models")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ASSISTANT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.WaitDelay = time.Second
	configureCursorProcess(cmd)
	output := &modelOutput{remaining: 256 << 10}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	if err := cmd.Run(); err != nil {
		// Never forward CLI stderr: it may contain credentials or private paths.
		return nil, errors.New("Cursor model discovery failed")
	}
	return parseCursorModels(output.String())
}

type modelOutput struct {
	bytes.Buffer
	remaining int
}

func (w *modelOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("model output exceeds limit")
	}
	w.remaining -= len(p)
	return w.Buffer.Write(p)
}

var modelANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
var cursorModelID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,199}$`)

func parseCursorModels(output string) ([]model.ModelOption, error) {
	var models []model.ModelOption
	seen := make(map[string]bool)
	for _, line := range strings.Split(modelANSI.ReplaceAllString(output, ""), "\n") {
		id, name, ok := strings.Cut(strings.TrimSpace(line), " - ")
		if !ok || !cursorModelID.MatchString(id) || seen[id] {
			continue
		}
		name = strings.TrimSpace(name)
		for _, suffix := range []string{" (current, default)", " (default, current)", " (current)", " (default)"} {
			name = strings.TrimSuffix(name, suffix)
		}
		if name == "" || len(name) > 300 || strings.ContainsAny(name, "\r\x1b\x00") {
			continue
		}
		seen[id] = true
		models = append(models, model.ModelOption{ID: id, Name: name})
		if len(models) == 256 {
			break
		}
	}
	if len(models) == 0 {
		return nil, errors.New("Cursor returned no recognizable models")
	}
	return models, nil
}
