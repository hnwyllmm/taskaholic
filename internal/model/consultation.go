package model

// TaskConsultation is a durable, read-only conversation about a task.  The
// execution task and Session are deliberately separate from SubjectTaskID so a
// question can never queue a Directive for, or change the state of, the worker.
type TaskConsultation struct {
	Session         *Session      `json:"session,omitempty"`
	ID              string        `json:"consultation_id"`
	SubjectTaskID   string        `json:"subject_task_id"`
	ExecutionTaskID string        `json:"execution_task_id"`
	State           string        `json:"state"`
	Version         int64         `json:"version"`
	Messages        []RoleMessage `json:"messages"`
	LastRunID       string        `json:"last_run_id,omitempty"`
	Error           string        `json:"error,omitempty"`
	SnapshotAtMS    int64         `json:"snapshot_at_ms,omitempty"`
	SnapshotCursor  int64         `json:"snapshot_cursor,omitempty"`
	CreatedAtMS     int64         `json:"created_at_ms"`
	UpdatedAtMS     int64         `json:"updated_at_ms"`
}
