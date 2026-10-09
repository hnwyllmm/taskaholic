package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"work-assistant/internal/model"
)

type ExecAdapter struct{}

func (ExecAdapter) Name() string { return "exec-agent" }

func (ExecAdapter) Capabilities() map[string]any {
	return map[string]any{
		"adapter":         "exec-agent",
		"interruptible":   true,
		"streaming_logs":  true,
		"argv_execution":  true,
		"live_directives": false,
		"session_memory":  "workspace",
	}
}

func (ExecAdapter) Run(ctx context.Context, spec model.RunSpec, workingDir string, directives <-chan model.Directive, emit func(Event)) Result {
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
	if err := command.Start(); err != nil {
		return Result{ExitCode: -1, Err: fmt.Errorf("start command: %w", err)}
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go scanOutput(stdout, "stdout", emit, &readers)
	go scanOutput(stderr, "stderr", emit, &readers)
	directiveDone := make(chan struct{})
	var directiveReader sync.WaitGroup
	if directives != nil {
		directiveReader.Add(1)
		go rejectLiveDirectives(directives, directiveDone, emit, &directiveReader)
	}
	// Cmd.Wait closes StdoutPipe/StderrPipe itself.  Waiting for it before the
	// scanners have drained their kernel buffers can discard a short final line
	// (and made stdout/stderr events nondeterministic under load).  The child
	// closes its descriptors on exit, so drain both streams first, then reap.
	readers.Wait()
	err = command.Wait()
	close(directiveDone)
	directiveReader.Wait()
	if err == nil {
		return Result{ExitCode: 0}
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return Result{ExitCode: exitError.ExitCode(), Err: err}
	}
	return Result{ExitCode: -1, Err: err}
}

func rejectLiveDirectives(directives <-chan model.Directive, done <-chan struct{}, emit func(Event), wait *sync.WaitGroup) {
	defer wait.Done()
	for {
		select {
		case <-done:
			return
		case directive, ok := <-directives:
			if !ok {
				return
			}
			emit(Event{
				Type: "directive.rejected", DirectiveID: directive.ID,
				Error: "exec-agent does not support live guidance; use an interactive Agent Adapter",
			})
		}
	}
}

func scanOutput(reader io.Reader, stream string, emit func(Event), wait *sync.WaitGroup) {
	defer wait.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		emit(Event{Message: scanner.Text(), Stream: stream})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		emit(Event{Message: "read " + stream + ": " + err.Error(), Stream: "runtime"})
	}
}
