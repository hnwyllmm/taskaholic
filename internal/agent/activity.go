package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"work-assistant/internal/model"
)

// Decode only public action fields. Reasoning/unknown raw payloads are not
// exposed as activity. Preserve provider snapshots rather than appending them.
func codexActivity(raw json.RawMessage, phase, scope string) *model.Action {
	var item struct {
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Status     string          `json:"status"`
		Text       string          `json:"text"`
		Command    string          `json:"command"`
		Cwd        string          `json:"cwd"`
		Output     string          `json:"aggregated_output"`
		ExitCode   *int            `json:"exit_code"`
		DurationMS *int64          `json:"duration_ms"`
		Server     string          `json:"server"`
		Tool       string          `json:"tool"`
		Arguments  json.RawMessage `json:"arguments"`
		Result     json.RawMessage `json:"result"`
		Error      json.RawMessage `json:"error"`
		Changes    json.RawMessage `json:"changes"`
		Items      json.RawMessage `json:"items"`
		Query      string          `json:"query"`
	}
	if json.Unmarshal(raw, &item) != nil || item.ID == "" {
		return nil
	}
	identity := item.ID
	if len(identity) > 128 {
		hash := sha256.Sum256([]byte(identity))
		identity = hex.EncodeToString(hash[:])
	}
	a := model.Action{ID: scope + "/" + identity, NativeID: item.ID, Kind: item.Type, State: "RUNNING", ExitCode: item.ExitCode, DurationMS: item.DurationMS}
	if phase == "item.completed" {
		a.State = "COMPLETED"
	}
	switch item.Status {
	case "completed":
		a.State = "COMPLETED"
	case "failed":
		a.State = "FAILED"
	case "declined", "cancelled", "interrupted":
		a.State = "INTERRUPTED"
	case "pending":
		a.State = "PENDING"
	}
	if item.ExitCode != nil && *item.ExitCode != 0 {
		a.State = "FAILED"
	}
	pretty := func(raw json.RawMessage) string {
		if len(raw) == 0 || string(raw) == "null" {
			return ""
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return ""
		}
		out, _ := json.MarshalIndent(v, "", "  ")
		return string(out)
	}
	switch item.Type {
	case "command_execution":
		a.Kind = "command"
		a.Title = "执行命令"
		a.Command = item.Command
		a.Details = item.Cwd
		a.Output = item.Output
	case "mcp_tool_call":
		a.Kind = "tool"
		a.Title = item.Server + " / " + item.Tool
		a.Details = pretty(item.Arguments)
		a.Output = pretty(item.Result)
		a.Error = pretty(item.Error)
	case "file_change":
		a.Kind = "file_change"
		a.Title = "文件变更"
		a.Details = pretty(item.Changes)
	case "web_search":
		a.Kind = "search"
		a.Title = "检索资料"
		a.Details = item.Query
	case "todo_list":
		a.Kind = "plan"
		a.Title = "工作计划"
		a.Details = pretty(item.Items)
	case "agent_message":
		a.Kind = "message"
		a.Title = "Agent 消息"
		a.Output = item.Text
	default:
		return nil
	}
	if a.Error != "" {
		a.State = "FAILED"
	}
	a = model.CleanAction(a)
	return &a
}
