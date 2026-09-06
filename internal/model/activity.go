package model

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Action is an observation, never a request to execute work or mutate a task.
// IDs are scoped by an adapter invocation; Output is a snapshot, not a delta.
type Action struct {
	ID         string `json:"action_id"`
	NativeID   string `json:"native_id,omitempty"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Title      string `json:"title"`
	Command    string `json:"command,omitempty"`
	Details    string `json:"details,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Redacted   bool   `json:"redacted,omitempty"`
}

type Activity struct {
	Action
	TaskID       string `json:"task_id"`
	RunID        string `json:"run_id"`
	SessionID    string `json:"session_id"`
	AgentID      string `json:"agent_id"`
	RuntimeID    string `json:"runtime_id"`
	ModelID      string `json:"model_id,omitempty"`
	FirstSeq     int64  `json:"first_seq"`
	LastSeq      int64  `json:"last_seq"`
	StartedAtMS  int64  `json:"started_at_ms"`
	UpdatedAtMS  int64  `json:"updated_at_ms"`
	FinishedAtMS int64  `json:"finished_at_ms,omitempty"`
}

type ActivityPage struct {
	Items      []Activity `json:"items"`
	Cursor     int64      `json:"cursor"`
	NextBefore int64      `json:"next_before"`
	HasMore    bool       `json:"has_more"`
}

func ActionActive(state string) bool { return state == "RUNNING" || state == "PENDING" }

var observationSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)((?:authorization|proxy-authorization)["']?\s*[:=]\s*["']?(?:bearer|basic)\s+)[^\s"']+`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret|token|cookie)["']?\s*(?::|=)\s*["']?)[^\s"',;}]+`),
	regexp.MustCompile(`(?i)(--(?:api-key|token|password|secret)\s+["']?)[^\s"']+`),
	regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{16,})`),
}

// Best-effort masking of common secret formats, not a complete DLP boundary.
// Called before the runtime spool AND again at the control persistence boundary.
func ObservationText(text string, limit int) (string, bool, bool) {
	original := text
	for i, re := range observationSecrets {
		if i == len(observationSecrets)-1 {
			text = re.ReplaceAllString(text, "[REDACTED]")
		} else {
			text = re.ReplaceAllString(text, "${1}[REDACTED]")
		}
	}
	redacted := text != original
	if len(text) <= limit {
		return text, false, redacted
	}
	const marker = "\n…（内容超过展示上限，已截断）"
	// Include the marker in the bound. Repeated sanitization (adapter, spool,
	// control) must not add extra newlines or truncate the marker repeatedly.
	cut := limit - len(marker)
	if cut < 0 {
		return "", true, redacted
	}
	text = text[:cut]
	for !utf8.ValidString(text) && len(text) > 0 {
		text = text[:len(text)-1]
	}
	return text + marker, true, redacted
}

func CleanAction(a Action) Action {
	clean := func(value *string, limit int) {
		text, truncated, redacted := ObservationText(*value, limit)
		*value = text
		a.Truncated = a.Truncated || truncated
		a.Redacted = a.Redacted || redacted
	}
	clean(&a.Title, 400)
	clean(&a.Command, 4000)
	clean(&a.Details, 8000)
	clean(&a.Output, 16000)
	clean(&a.Error, 2000)
	clean(&a.NativeID, 256)
	switch a.State {
	case "PENDING", "RUNNING", "COMPLETED", "FAILED", "INTERRUPTED", "UNKNOWN":
	default:
		a.State = "UNKNOWN"
	}
	if a.DurationMS != nil && *a.DurationMS < 0 {
		a.DurationMS = nil
	}
	if strings.TrimSpace(a.Title) == "" {
		a.Title = "Agent 行动"
	}
	return a
}
