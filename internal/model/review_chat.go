package model

// ReviewTurn is a durable question/answer bound to one immutable submission.
// Native memory remains owned by the original execution adapter.
type ReviewTurn struct {
	ID           string `json:"turn_id"`
	ReviewID     string `json:"review_id"`
	TaskID       string `json:"task_id"`
	SourceRunID  string `json:"source_run_id"`
	SessionID    string `json:"session_id"`
	AgentID      string `json:"agent_id"`
	RuntimeID    string `json:"runtime_id"`
	AdapterID    string `json:"adapter_id"`
	ModelID      string `json:"model_id,omitempty"`
	State        string `json:"state"`
	Question     string `json:"question"`
	Answer       string `json:"answer,omitempty"`
	Error        string `json:"error,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	CreatedAtMS  int64  `json:"created_at_ms"`
	FinishedAtMS int64  `json:"finished_at_ms,omitempty"`
}
