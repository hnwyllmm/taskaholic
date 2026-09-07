package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
)

const codexSessionPrefix = "codex:"

// CodexAdapter runs the stable, non-interactive Codex CLI and maps a control
// Session to a persisted Codex thread. A message Directive received while a
// turn is running is applied as a follow-up turn after that turn finishes.
type CodexAdapter struct {
	binary  string
	sandbox string
}

func NewCodexAdapter(binary, sandbox string) (*CodexAdapter, error) {
	if strings.TrimSpace(binary) == "" {
		binary = "codex"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("find Codex CLI %q: %w", binary, err)
	}
	switch sandbox {
	case "":
		sandbox = "workspace-write"
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return nil, fmt.Errorf("unsupported Codex sandbox %q", sandbox)
	}
	return &CodexAdapter{binary: resolved, sandbox: sandbox}, nil
}

func (a *CodexAdapter) Name() string { return "codex-agent" }

func (a *CodexAdapter) Capabilities() map[string]any {
	return map[string]any{
		"adapter":             a.Name(),
		"binary":              a.binary,
		"interruptible":       true,
		"streaming_logs":      true,
		"structured_activity": true,
		"live_directives":     true,
		"directive_mode":      "next-turn",
		"native_session":      true,
		"per_session_model":   true,
		"task_goal_as_prompt": true,
		"sandbox":             a.sandbox,
		"role_instructions":   true,
		"structured_output":   true,
		"read_only_runs":      true,
		"reasoning_effort":    true,
	}
}

func (a *CodexAdapter) Run(ctx context.Context, spec model.RunSpec, workingDir string, directives <-chan model.Directive, emit func(Event)) Result {
	if spec.ReadOnly {
		readOnly := *a
		readOnly.sandbox = "read-only"
		a = &readOnly
	}
	prompt := codexPrompt(spec)
	if prompt == "" {
		return Result{ExitCode: -1, Err: errors.New("Codex task prompt is empty")}
	}

	sessionID, valid := parseCodexSessionRef(spec.AgentSessionRef)
	if !valid && (spec.RequireNativeSession || (spec.AgentSessionRef != "" && !strings.HasPrefix(spec.AgentSessionRef, "workspace:"))) {
		return Result{ExitCode: -1, Err: errors.New("invalid stored Codex session reference; refusing to start a replacement session")}
	}
	var err error
	spec, err = prepareReasoning(ctx, a, spec)
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	mismatchedSession := false
	guardedEmit := func(event Event) {
		if spec.RequireNativeSession && event.Type == "session.bound" && event.AgentSessionRef != spec.AgentSessionRef {
			mismatchedSession = true
			return
		}
		emit(event)
	}
	result := a.runTurn(ctx, workingDir, spec.ModelID, spec.ReasoningEffort, sessionID, prompt, guardedEmit, func() {
		emit(Event{Type: "run.configured", Execution: &spec.ExecutionSettings})
	}, spec.OutputSchema)
	if mismatchedSession {
		return Result{ExitCode: -1, Err: errors.New("Codex returned a different native session; original session binding preserved")}
	}
	if result.SessionID != "" {
		sessionID = result.SessionID
	}
	if result.Err != nil {
		return Result{ExitCode: result.ExitCode, Err: result.Err}
	}
	for {
		select {
		case <-ctx.Done():
			return Result{ExitCode: -1, Err: ctx.Err()}
		case directive, ok := <-directives:
			if !ok {
				return Result{ExitCode: 0, Output: result.Output}
			}
			if directive.Kind != model.DirectiveKindMessage || strings.TrimSpace(directive.Message) == "" {
				emit(Event{Type: "directive.rejected", DirectiveID: directive.ID, Error: "codex-agent accepts non-empty message directives"})
				continue
			}
			if sessionID == "" {
				emit(Event{Type: "directive.rejected", DirectiveID: directive.ID, Error: "Codex did not return a resumable session id"})
				continue
			}
			followUp := a.runTurn(ctx, workingDir, spec.ModelID, spec.ReasoningEffort, sessionID, spec.Instructions+"\n\n"+directive.Message, guardedEmit, func() {
				emit(Event{Type: "directive.applied", DirectiveID: directive.ID})
			}, spec.OutputSchema)
			if mismatchedSession {
				return Result{ExitCode: -1, Err: errors.New("Codex returned a different native session; original session binding preserved")}
			}
			if followUp.SessionID != "" {
				sessionID = followUp.SessionID
			}
			if followUp.Err != nil {
				return Result{ExitCode: followUp.ExitCode, Err: followUp.Err}
			}
			result = followUp
		default:
			return Result{ExitCode: 0, Output: result.Output}
		}
	}
}

type codexTurnResult struct {
	Output    string
	SessionID string
	ExitCode  int
	Err       error
}

func (a *CodexAdapter) runTurn(ctx context.Context, workingDir, modelID, effort, sessionID, prompt string, emit func(Event), started func(), schemas ...json.RawMessage) codexTurnResult {
	args := []string{"exec", "-c", "approval_policy=\"never\""}
	if effort != "" {
		args = append(args, "-c", "model_reasoning_effort=\""+effort+"\"")
	}
	var schemaPath string
	if len(schemas) > 0 && len(schemas[0]) > 0 {
		file, err := os.CreateTemp("", "work-assistant-schema-*.json")
		if err != nil {
			return codexTurnResult{ExitCode: -1, Err: err}
		}
		schemaPath = file.Name()
		defer os.Remove(schemaPath)
		_, writeErr := file.Write(schemas[0])
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return codexTurnResult{ExitCode: -1, Err: err}
		}
	}
	if sessionID == "" {
		args = append(args, "--json", "--color", "never", "--sandbox", a.sandbox,
			"--skip-git-repo-check")
		if modelID != "" {
			args = append(args, "--model", modelID)
		}
		if schemaPath != "" {
			args = append(args, "--output-schema", schemaPath)
		}
		args = append(args, "--cd", workingDir, prompt)
	} else {
		args = append(args, "resume", "--json", "--skip-git-repo-check", "-c", "sandbox_mode=\""+a.sandbox+"\"")
		if modelID != "" {
			args = append(args, "--model", modelID)
		}
		if schemaPath != "" {
			args = append(args, "--output-schema", schemaPath)
		}
		args = append(args, sessionID, prompt)
	}

	command := exec.CommandContext(ctx, a.binary, args...)
	command.Dir = workingDir
	command.Stdin = nil
	configureCodexProcess(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return codexTurnResult{ExitCode: -1, Err: fmt.Errorf("open Codex stdout: %w", err)}
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return codexTurnResult{ExitCode: -1, Err: fmt.Errorf("open Codex stderr: %w", err)}
	}
	if err := command.Start(); err != nil {
		return codexTurnResult{ExitCode: -1, Err: fmt.Errorf("start Codex CLI: %w", err)}
	}
	if started != nil {
		started()
	}

	var stderrReaders sync.WaitGroup
	stderrReaders.Add(1)
	go scanCodexStderr(stderr, emit, &stderrReaders)
	parsed := scanCodexEvents(stdout, emit)
	waitErr := command.Wait()
	stderrReaders.Wait()
	if ctx.Err() != nil {
		return codexTurnResult{SessionID: parsed.SessionID, ExitCode: -1, Err: ctx.Err()}
	}
	if parsed.Err != nil {
		return codexTurnResult{SessionID: parsed.SessionID, ExitCode: exitCode(waitErr), Err: parsed.Err}
	}
	if waitErr != nil {
		return codexTurnResult{SessionID: parsed.SessionID, ExitCode: exitCode(waitErr), Err: fmt.Errorf("Codex CLI: %w", waitErr)}
	}
	return codexTurnResult{SessionID: parsed.SessionID, ExitCode: 0, Output: parsed.Output}
}

// Do not pass the control-plane credentials to the CLI or its tools. Kill the
// entire process group on interruption so child commands cannot outlive a run.
func configureCodexProcess(command *exec.Cmd) {
	command.Env = []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ASSISTANT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.WaitDelay = time.Second
	configureCursorProcess(command) // Shared platform-specific process-group setup.
}

func codexPrompt(spec model.RunSpec) string {
	var sections []string
	if strings.TrimSpace(spec.Instructions) != "" {
		sections = append(sections, spec.Instructions)
	}
	if strings.TrimSpace(spec.TaskTitle) != "" {
		sections = append(sections, "Task: "+strings.TrimSpace(spec.TaskTitle))
	}
	if strings.TrimSpace(spec.TaskGoal) != "" {
		sections = append(sections, "Goal:\n"+strings.TrimSpace(spec.TaskGoal))
	}
	if len(spec.Command) > 0 {
		instruction := strings.TrimSpace(strings.Join(spec.Command, " "))
		if instruction != "" {
			sections = append(sections, "Additional instructions:\n"+instruction)
		}
	}
	return strings.Join(sections, "\n\n")
}

func parseCodexSessionRef(reference string) (string, bool) {
	sessionID, ok := strings.CutPrefix(strings.TrimSpace(reference), codexSessionPrefix)
	if !ok {
		return "", false
	}
	sessionID = strings.TrimSpace(sessionID)
	return sessionID, sessionID != ""
}

type parsedCodexTurn struct {
	Output    string
	SessionID string
	Err       error
}

func scanCodexEvents(reader io.Reader, emit func(Event)) parsedCodexTurn {
	var result parsedCodexTurn
	scope := id.New("observation")
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		var envelope struct {
			Type     string          `json:"type"`
			ThreadID string          `json:"thread_id"`
			Item     json.RawMessage `json:"item"`
			Error    json.RawMessage `json:"error"`
			Message  string          `json:"message"`
			Usage    json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			emit(Event{Message: line, Stream: "codex"})
			continue
		}
		switch envelope.Type {
		case "thread.started":
			if envelope.ThreadID != "" {
				result.SessionID = envelope.ThreadID
				emit(Event{Type: "session.bound", AgentSessionRef: codexSessionPrefix + envelope.ThreadID})
			}
		case "item.completed", "item.started", "item.updated":
			if action := codexActivity(envelope.Item, envelope.Type, scope); action != nil {
				// Keep a bounded text view for existing consumers such as candidate
				// upgrade logs; the task UI consumes the structured snapshot.
				message, _, _ := model.ObservationText(strings.Join([]string{action.Title, action.Command, action.Details, action.Output, action.Error}, "\n"), 16000)
				emit(Event{Activity: action, Stream: "activity", Message: strings.TrimSpace(message)})
			} else {
				emitCodexItem(envelope.Item, emit)
			}
			if envelope.Type == "item.completed" {
				var item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(envelope.Item, &item) == nil && item.Type == "agent_message" {
					result.Output = item.Text
				}
			}
		case "turn.completed":
			if len(envelope.Usage) > 0 {
				emit(Event{Message: string(envelope.Usage), Stream: "codex-usage"})
			}
		case "turn.failed", "error":
			message := codexErrorMessage(envelope.Message, envelope.Error)
			if message != "" {
				result.Err = errors.New(message)
				emit(Event{Message: message, Stream: "codex-error"})
			}
		}
	}
	if err := scanner.Err(); err != nil && result.Err == nil {
		result.Err = fmt.Errorf("read Codex JSONL: %w", err)
	}
	return result
}

func emitCodexItem(raw json.RawMessage, emit func(Event)) {
	if len(raw) == 0 {
		return
	}
	var item struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		Message          string `json:"message"`
	}
	if json.Unmarshal(raw, &item) != nil {
		emit(Event{Message: string(raw), Stream: "codex"})
		return
	}
	switch item.Type {
	case "reasoning":
		// Action observation does not expose hidden/internal reasoning payloads.
		return
	case "agent_message":
		if item.Text != "" {
			emit(Event{Message: item.Text, Stream: "agent"})
		}
	case "command_execution":
		if item.Command != "" {
			emit(Event{Message: item.Command, Stream: "codex-command"})
		}
		if item.AggregatedOutput != "" {
			emit(Event{Message: item.AggregatedOutput, Stream: "codex-command-output"})
		}
	case "error":
		if item.Message != "" {
			emit(Event{Message: item.Message, Stream: "codex-error"})
		}
	default:
		if item.Text != "" {
			emit(Event{Message: item.Text, Stream: "codex"})
		}
	}
}

func codexErrorMessage(message string, raw json.RawMessage) string {
	if strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "Codex turn failed"
	}
	var object struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Message != "" {
		return object.Message
	}
	return string(raw)
}

func scanCodexStderr(reader io.Reader, emit func(Event), wait *sync.WaitGroup) {
	defer wait.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		emit(Event{Message: scanner.Text(), Stream: "codex-stderr"})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		emit(Event{Message: err.Error(), Stream: "codex-stderr"})
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}
