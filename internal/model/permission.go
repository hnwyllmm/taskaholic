package model

// Execution policies never grant shell privileges or replace plan approval.
type ExecutionPolicy struct {
	ID         string `json:"policy_id"`
	Operation  string `json:"operation"`
	RuntimeID  string `json:"runtime_id"`
	Repository string `json:"repository"`
	Effect     string `json:"effect"` // allow, ask, deny
	Version    int64  `json:"version"`
}
type PermissionRequest struct {
	ID           string `json:"request_id"`
	TaskID       string `json:"task_id"`
	ParentTaskID string `json:"parent_task_id,omitempty"`
	Title        string `json:"title"`
	AgentID      string `json:"agent_id"`
	RuntimeID    string `json:"runtime_id"`
	Operation    string `json:"operation"`
	Repository   string `json:"repository"`
	PlanHash     string `json:"plan_hash"`
	ReviewID     string `json:"review_id"`
	Fingerprint  string `json:"fingerprint"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	Version      int64  `json:"version"`
	TaskVersion  int64  `json:"task_version"`
	CreatedAtMS  int64  `json:"created_at_ms"`
}
type ExecutionCapability struct {
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
	CheckedAtMS int64  `json:"checked_at_ms,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}
