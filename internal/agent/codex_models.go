package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"work-assistant/internal/model"
)

// ListModels uses the installed Codex account's picker catalog. This short-lived
// stdio connection never starts/resumes a thread or sends a prompt to a model.
// Protocol: https://developers.openai.com/codex/app-server#models
func (a *CodexAdapter) ListModels(parent context.Context) ([]model.ModelOption, error) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.binary, "app-server", "--listen", "stdio://")
	configureCodexProcess(cmd)
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("Codex model discovery unavailable")
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("Codex model discovery unavailable")
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		return nil, errors.New("Codex model discovery unavailable")
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	return readCodexModels(stdin, stdout)
}

func readCodexModels(input io.Writer, output io.Reader) ([]model.ModelOption, error) {
	// Bound the whole exchange as well as individual JSONL messages. Never expose
	// raw protocol errors/stdout/stderr: these can contain private account details.
	failure := errors.New("Codex model discovery failed")
	encoder := json.NewEncoder(input)
	scanner := bufio.NewScanner(io.LimitReader(output, 1<<20))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	send := func(id int, method string, params any) error {
		return encoder.Encode(map[string]any{"id": id, "method": method, "params": params})
	}
	receive := func(id int) (json.RawMessage, error) {
		for scanner.Scan() {
			var msg struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &msg) != nil {
				return nil, failure
			}
			if msg.ID == nil { // Unsolicited notifications are not catalog entries.
				continue
			}
			if msg.Method != "" || *msg.ID != id || (len(msg.Error) != 0 && string(msg.Error) != "null") || len(msg.Result) == 0 || string(msg.Result) == "null" {
				return nil, failure
			}
			return msg.Result, nil
		}
		return nil, failure
	}
	if send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "work_assistant", "version": "0.1.0"}}) != nil {
		return nil, failure
	}
	if _, err := receive(1); err != nil {
		return nil, err
	}
	if encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}) != nil {
		return nil, failure
	}
	var models []model.ModelOption
	seen, cursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	for requestID := 2; requestID < 18; requestID++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if send(requestID, "model/list", params) != nil {
			return nil, failure
		}
		raw, err := receive(requestID)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data []struct {
				ID          string `json:"id"`
				Model       string `json:"model"`
				DisplayName string `json:"displayName"`
				Hidden      bool   `json:"hidden"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil {
			return nil, failure
		}
		for _, entry := range page.Data {
			id := entry.Model
			if id == "" {
				id = entry.ID
			}
			if entry.Hidden || !cursorModelID.MatchString(id) || seen[id] {
				continue
			}
			name := strings.TrimSpace(entry.DisplayName)
			if name == "" {
				name = id
			}
			if len(name) > 300 || strings.ContainsAny(name, "\r\n\x1b\x00") {
				continue
			}
			seen[id] = true
			models = append(models, model.ModelOption{ID: id, Name: name})
			if len(models) > 256 {
				return nil, failure
			}
		}
		if page.NextCursor == "" {
			if len(models) == 0 {
				return nil, failure
			}
			return models, nil
		}
		if cursors[page.NextCursor] {
			return nil, failure
		}
		cursor, cursors[page.NextCursor] = page.NextCursor, true
	}
	return nil, failure
}
