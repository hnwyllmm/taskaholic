package model

// EnvironmentExecution is issued by the Manager, never accepted from model prose.
type EnvironmentExecution struct {
	ParentTaskID    string         `json:"parent_task_id"`
	SourceSessionID string         `json:"source_session_id"`
	SourceRunID     string         `json:"source_run_id"`
	Profile         string         `json:"profile"`
	Grant           ExecutionGrant `json:"grant"`
}

type EnvironmentResult struct {
	Status         string `json:"status"` // passed, failed, unavailable, interrupted
	Profile        string `json:"profile"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Message        string `json:"message"`
	Log            string `json:"log"`
}
