package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"work-assistant/internal/model"
)

// StdioAdapter is a small reference protocol for an interactive agent process.
// The process receives one JSON object per directive on stdin and owns all
// higher-level interpretation and memory inside its session workspace.
type StdioAdapter struct{}

func (StdioAdapter) Name() string { return "stdio-agent" }

func (StdioAdapter) Capabilities() map[string]any {
	return map[string]any{
		"adapter":          "stdio-agent",
		"interruptible":    true,
		"streaming_logs":   true,
		"argv_execution":   true,
		"live_directives":  true,
		"directive_format": "ndjson/stdin",
		"session_memory":   "agent-owned-workspace",
	}
}

func (StdioAdapter) Run(ctx context.Context, spec model.RunSpec, workingDir string, directives <-chan model.Directive, emit func(Event)) Result {
	if len(spec.Command) == 0 {
		return Result{ExitCode: -1, Err: errors.New("empty command")}
	}
	command := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	command.Dir = workingDir
	stdout, err := command.StdoutPipe()
	if err != nil {
		return Result{ExitCode: -1, Err: fmt.Errorf("open stdout: %w", err)}
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return Result{ExitCode: -1, Err: fmt.Errorf("open stderr: %w", err)}
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return Result{ExitCode: -1, Err: fmt.Errorf("open stdin: %w", err)}
	}
	if err := command.Start(); err != nil {
		return Result{ExitCode: -1, Err: fmt.Errorf("start command: %w", err)}
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go scanOutput(stdout, "stdout", emit, &readers)
	go scanOutput(stderr, "stderr", emit, &readers)
	directiveDone := make(chan struct{})
	var directiveWriter sync.WaitGroup
	directiveWriter.Add(1)
	go func() {
		defer directiveWriter.Done()
		encoder := json.NewEncoder(stdin)
		for {
			select {
			case <-directiveDone:
				return
			case directive, ok := <-directives:
				if !ok {
					return
				}
				wire := map[string]any{
					"type": "directive", "directive_id": directive.ID,
					"kind": directive.Kind, "message": directive.Message,
				}
				if err := encoder.Encode(wire); err != nil {
					emit(Event{Type: "directive.rejected", DirectiveID: directive.ID, Error: err.Error()})
					continue
				}
				emit(Event{Type: "directive.applied", DirectiveID: directive.ID})
			}
		}
	}()
	err = command.Wait()
	readers.Wait()
	close(directiveDone)
	directiveWriter.Wait()
	_ = stdin.Close()
	if err == nil {
		return Result{ExitCode: 0}
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return Result{ExitCode: exitError.ExitCode(), Err: err}
	}
	return Result{ExitCode: -1, Err: err}
}
