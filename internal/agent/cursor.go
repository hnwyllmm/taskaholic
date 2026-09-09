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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
)

// CursorAdapter uses public CLI NDJSON, ask mode, and native chat IDs. It does
// not grant workspace writes, force tool approvals, or enable external MCPs.
type CursorAdapter struct{ binary string }

func NewCursorAdapter(binary string) (*CursorAdapter, error) {
	if strings.TrimSpace(binary) == "" {
		binary = "agent"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("find Cursor Agent %q: %w", binary, err)
	}
	return &CursorAdapter{binary: resolved}, nil
}
func (a *CursorAdapter) Name() string { return "cursor-agent" }
func (a *CursorAdapter) Capabilities() map[string]any {
	return map[string]any{
		"adapter": a.Name(), "binary": a.binary, "interruptible": true, "streaming_logs": true, "structured_activity": true,
		"live_directives": true, "directive_mode": "next-turn", "native_session": true, "per_session_model": true,
		"task_goal_as_prompt": true, "role_instructions": true, "structured_output": true, "structured_output_mode": "prompt-and-validated-json",
		"read_only_runs": true, "execution_mode": "ask", "sandbox": "enabled",
		"reasoning_effort": true,
	}
}

var cursorChatID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func parseCursorSessionRef(ref string) (string, bool) {
	native, ok := strings.CutPrefix(ref, "cursor:")
	return native, ok && cursorChatID.MatchString(native)
}

func cursorPrompt(spec model.RunSpec) string {
	prompt := codexPrompt(spec) // Shared role/task text; this helper has no CLI flags.
	if len(spec.OutputSchema) > 0 {
		prompt += "\n\nOUTPUT PROTOCOL: Return exactly one JSON object matching this schema as your FINAL assistant message. No markdown fences, preamble, or trailing text. Keep tool progress separate from the final JSON. Files are delivered as JSON text; do not write them to disk.\nJSON schema:\n" + string(spec.OutputSchema)
	}
	return prompt
}

func (a *CursorAdapter) Run(ctx context.Context, spec model.RunSpec, directory string, directives <-chan model.Directive, emit func(Event)) Result {
	if strings.TrimSpace(codexPrompt(spec)) == "" {
		return Result{ExitCode: -1, Err: errors.New("Cursor task prompt is empty")}
	}
	native, valid := parseCursorSessionRef(spec.AgentSessionRef)
	if !valid {
		if spec.RequireNativeSession || (spec.AgentSessionRef != "" && !strings.HasPrefix(spec.AgentSessionRef, "workspace:")) {
			return Result{ExitCode: -1, Err: errors.New("invalid stored Cursor session; refusing to create a replacement")}
		}
		native = ""
	}
	var err error
	spec, err = prepareReasoning(ctx, a, spec)
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	result := a.runTurn(ctx, spec, directory, native, emit, func() {
		emit(Event{Type: "run.configured", Execution: &spec.ExecutionSettings})
	})
	if result.Err != nil {
		return Result{ExitCode: result.ExitCode, Err: result.Err}
	}
	native = result.SessionID
	for {
		select {
		case <-ctx.Done():
			return Result{ExitCode: -1, Err: ctx.Err()}
		case d, ok := <-directives:
			if !ok {
				return Result{Output: result.Output}
			}
			if d.Kind != model.DirectiveKindMessage || strings.TrimSpace(d.Message) == "" {
				emit(Event{Type: "directive.rejected", DirectiveID: d.ID, Error: "cursor-agent accepts non-empty message directives"})
				continue
			}
			follow := spec
			follow.TaskTitle = ""
			follow.TaskGoal = d.Message
			follow.Command = nil
			result = a.runTurn(ctx, follow, directory, native, emit, func() { emit(Event{Type: "directive.applied", DirectiveID: d.ID}) })
			if result.Err != nil {
				return Result{ExitCode: result.ExitCode, Err: result.Err}
			}
		default:
			return Result{Output: result.Output}
		}
	}
}

func (a *CursorAdapter) runTurn(ctx context.Context, spec model.RunSpec, directory, native string, emit func(Event), started func()) codexTurnResult {
	if err := ensureCursorPolicy(directory); err != nil {
		return codexTurnResult{ExitCode: -1, Err: err}
	}
	// Always ask/read-only, even for a generic caller. --force/--yolo and
	// --approve-mcps are intentionally absent. Never fall back without sandbox.
	args := []string{"--print", "--mode", "ask", "--sandbox", "enabled", "--output-format", "stream-json", "--workspace", directory, "--trust"}
	if native != "" {
		args = append(args, "--resume", native)
	}
	if spec.ModelID != "" {
		args = append(args, "--model", spec.ModelID)
	}
	args = append(args, "--", cursorPrompt(spec))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, a.binary, args...)
	cmd.Dir = directory
	// Runtime/control credentials belong to this host process, not the Agent.
	// Preserve Cursor's own authentication and normal executable environment.
	for _, entry := range os.Environ() {
		if !isControlCredential(entry) {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.WaitDelay = 3 * time.Second
	configureCursorProcess(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return codexTurnResult{ExitCode: -1, Err: err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return codexTurnResult{ExitCode: -1, Err: err}
	}
	if err = cmd.Start(); err != nil {
		return codexTurnResult{ExitCode: -1, Err: fmt.Errorf("start Cursor Agent: %w", err)}
	}
	if started != nil {
		started()
	}
	var readers sync.WaitGroup
	readers.Add(1)
	var diagnostics strings.Builder
	go func() {
		defer readers.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			line, _, _ := model.ObservationText(scanner.Text(), 2000)
			if diagnostics.Len() < 4000 {
				diagnostics.WriteString(line + "\n")
			}
			emit(Event{Stream: "cursor-stderr", Message: line})
		}
	}()
	parsed := scanCursorEvents(stdout, native, len(spec.OutputSchema) > 0, emit)
	if parsed.Err != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	readers.Wait()
	if ctx.Err() != nil {
		return codexTurnResult{SessionID: parsed.SessionID, ExitCode: -1, Err: ctx.Err()}
	}
	if parsed.Err != nil {
		return codexTurnResult{SessionID: parsed.SessionID, ExitCode: -1, Err: fmt.Errorf("Cursor Agent: %w; %s", parsed.Err, strings.TrimSpace(diagnostics.String()))}
	}
	if waitErr != nil {
		return codexTurnResult{SessionID: parsed.SessionID, ExitCode: exitCode(waitErr), Err: fmt.Errorf("Cursor Agent: %w; %s", waitErr, strings.TrimSpace(diagnostics.String()))}
	}
	return codexTurnResult{SessionID: parsed.SessionID, Output: parsed.Output}
}

// This adapter owns only its isolated Session workspace, never the user's
// global Cursor configuration. Deny rules take priority over global allowlists.
// Refuse to overwrite an existing or symlinked project policy.
const cursorReadOnlyPolicy = `{"permissions":{"allow":[],"deny":["Write(**)","Shell(*)","Mcp(*:*)"]}}` + "\n"

func ensureCursorPolicy(directory string) error {
	dir := filepath.Join(directory, ".cursor")
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create Cursor workspace policy: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Cursor policy directory is not a real directory")
	}
	path := filepath.Join(dir, "cli.json")
	if info, err = os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("Cursor workspace policy is not a regular file")
		}
		body, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if string(body) != cursorReadOnlyPolicy {
			return errors.New("existing Cursor workspace policy differs; refusing to overwrite it")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(cursorReadOnlyPolicy)
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func scanCursorEvents(reader io.Reader, expected string, requireJSON bool, emit func(Event)) parsedCodexTurn {
	var parsed parsedCodexTurn
	scope := id.New("cursor_observation")
	messageNumber := 0
	lastMessage := ""
	terminal := false
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var e struct {
			Type      string          `json:"type"`
			Subtype   string          `json:"subtype"`
			SessionID string          `json:"session_id"`
			CallID    string          `json:"call_id"`
			IsError   bool            `json:"is_error"`
			Result    string          `json:"result"`
			Error     json.RawMessage `json:"error"`
			ToolCall  json.RawMessage `json:"tool_call"`
			Usage     json.RawMessage `json:"usage"`
			Message   struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			parsed.Err = fmt.Errorf("invalid Cursor NDJSON: %w", err)
			return parsed
		}
		if e.SessionID != "" {
			if !cursorChatID.MatchString(e.SessionID) || (expected != "" && e.SessionID != expected) || (parsed.SessionID != "" && e.SessionID != parsed.SessionID) {
				parsed.Err = errors.New("Cursor returned a different or invalid native session; original binding preserved")
				return parsed
			}
			if parsed.SessionID == "" {
				parsed.SessionID = e.SessionID
				emit(Event{Type: "session.bound", AgentSessionRef: "cursor:" + e.SessionID})
			}
		}
		if usage, ok := parseTokenUsage(e.Usage, "cursor"); ok {
			emit(Event{Usage: &usage, Stream: "usage"})
		}
		switch e.Type {
		case "assistant":
			var text strings.Builder
			for _, c := range e.Message.Content {
				if c.Type == "text" {
					text.WriteString(c.Text)
				}
			}
			if text.Len() > 128*1024 {
				parsed.Err = errors.New("Cursor message exceeds 128 KiB")
				return parsed
			}
			if text.Len() > 0 {
				lastMessage = text.String()
				messageNumber++
				a := model.CleanAction(model.Action{ID: fmt.Sprintf("%s/message-%d", scope, messageNumber), Kind: "message", State: "COMPLETED", Title: "Cursor 消息", Output: lastMessage})
				emit(Event{Activity: &a, Stream: "activity", Message: a.Output})
			}
		case "tool_call":
			if a := cursorToolActivity(e.ToolCall, e.Subtype, e.CallID, scope); a != nil {
				emit(Event{Activity: a, Stream: "activity", Message: a.Title})
			}
		case "result":
			terminal = true
			if e.IsError || e.Subtype != "success" {
				message, _, _ := model.ObservationText(e.Result+" "+string(e.Error), 2000)
				parsed.Err = fmt.Errorf("Cursor turn failed: %s", message)
				return parsed
			}
			parsed.Output = strings.TrimSpace(e.Result)
			// Cursor's result may concatenate commentary and the final JSON. Only
			// accept a complete final object, never scan arbitrary braces in prose.
			if requireJSON && !cursorJSONObject(parsed.Output) {
				if cursorJSONObject(lastMessage) {
					parsed.Output = strings.TrimSpace(lastMessage)
				} else {
					parsed.Err = errors.New("Cursor did not return the required final JSON object")
					return parsed
				}
			}
		case "error":
			message, _, _ := model.ObservationText(e.Result+" "+string(e.Error), 2000)
			parsed.Err = fmt.Errorf("Cursor error: %s", message)
			return parsed
		}
	}
	if err := scanner.Err(); err != nil {
		parsed.Err = fmt.Errorf("read Cursor NDJSON: %w", err)
	} else if !terminal || parsed.SessionID == "" {
		parsed.Err = errors.New("Cursor stream ended without a successful result and native session")
	}
	if len(parsed.Output) > 128*1024 {
		parsed.Err = errors.New("Cursor result exceeds 128 KiB")
	}
	return parsed
}
func cursorJSONObject(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "{") && json.Valid([]byte(text))
}

func cursorToolActivity(raw json.RawMessage, phase, callID, scope string) *model.Action {
	if callID == "" {
		return nil
	}
	var tools map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || len(tools) != 1 {
		return nil
	}
	for name, rawTool := range tools {
		var tool struct {
			Args   json.RawMessage `json:"args"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(rawTool, &tool) != nil {
			return nil
		}
		a := model.Action{ID: scope + "/" + callID, NativeID: callID, Kind: "tool", Title: name, State: "RUNNING"}
		if len(a.ID) > 256 {
			return nil
		}
		pretty := func(raw json.RawMessage) string {
			var v any
			if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil {
				return ""
			}
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		}
		a.Details = pretty(tool.Args)
		a.Output = pretty(tool.Result)
		switch name {
		case "shellToolCall":
			a.Kind = "command"
			a.Title = "执行命令"
			var args struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(tool.Args, &args)
			a.Command = args.Command
		case "readToolCall":
			a.Title = "读取文件"
		case "writeToolCall", "editToolCall", "applyPatchToolCall":
			a.Kind = "file_change"
			a.Title = "文件变更"
		case "webSearchToolCall", "webFetchToolCall":
			a.Kind = "search"
			a.Title = "检索资料"
		case "updateTodosToolCall":
			a.Kind = "plan"
			a.Title = "工作计划"
		}
		if phase == "completed" {
			a.State = "COMPLETED"
		}
		var result map[string]json.RawMessage
		_ = json.Unmarshal(tool.Result, &result)
		for _, key := range []string{"error", "failure", "rejected"} {
			if v := pretty(result[key]); v != "" {
				a.State = "FAILED"
				a.Error = v
			}
		}
		var success struct {
			ExitCode      *int   `json:"exitCode"`
			SnakeExitCode *int   `json:"exit_code"`
			Output        string `json:"output"`
		}
		_ = json.Unmarshal(result["success"], &success)
		a.ExitCode = success.ExitCode
		if a.ExitCode == nil {
			a.ExitCode = success.SnakeExitCode
		}
		if a.ExitCode != nil && *a.ExitCode != 0 {
			a.State = "FAILED"
		}
		a = model.CleanAction(a)
		return &a
	}
	return nil
}
