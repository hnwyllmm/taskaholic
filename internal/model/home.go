package model

// HomeChat is a presentation-layer conversation. Its internal execution task is
// not managed work. Native session memory still belongs to the runtime adapter.
type HomeChat struct {
	Session      *Session         `json:"session,omitempty"` // Read projection, not native memory.
	ID           string           `json:"chat_id"`
	TaskID       string           `json:"task_id"`
	Title        string           `json:"title"`
	State        string           `json:"state"`
	Version      int64            `json:"version"`
	Messages     []RoleMessage    `json:"messages"`
	Proposals    []HomeProposal   `json:"proposals"`
	LastRunID    string           `json:"last_run_id,omitempty"`
	Error        string           `json:"error,omitempty"`
	TaskVersions map[string]int64 `json:"task_versions,omitempty"`
	RoleVersions map[string]int64 `json:"role_versions,omitempty"`
	CreatedAtMS  int64            `json:"created_at_ms"`
	UpdatedAtMS  int64            `json:"updated_at_ms"`
}

type HomeAction struct {
	Kind         string    `json:"kind"`
	Title        string    `json:"title"`
	Text         string    `json:"text"`
	TargetTaskID string    `json:"target_task_id"`
	RoleID       string    `json:"role_id"`
	RoleSpec     *RoleSpec `json:"role_spec"`
}

type HomeProposal struct {
	ID string `json:"proposal_id"`
	HomeAction
	State         string `json:"state"`
	RunID         string `json:"run_id"`
	TargetVersion int64  `json:"target_version,omitempty"`
	RoleVersion   int64  `json:"role_version,omitempty"`
	ResourceID    string `json:"resource_id,omitempty"`
}
