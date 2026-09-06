package model

const (
	TaskStateQueued = "QUEUED"
	TaskStateReview = "WAITING_REVIEW"
	TaskStateInput  = "WAITING_INPUT"
	TaskStatePaused = "PAUSED"
)

// Project is the legacy storage/API name for team material shared by people and
// agents. It is context, not checkout permission or native session memory. Tasks
// keep a snapshot; new material never silently changes an existing session.
type Project struct {
	ID      string `json:"project_id"`
	Name    string `json:"name"`
	Context string `json:"context"`
	Version int64  `json:"version"`
}

type WorkConfig struct {
	TaskID         string  `json:"task_id"`
	AgentID        string  `json:"preferred_agent_id,omitempty"`
	Project        Project `json:"project"`
	Paused         bool    `json:"paused"`
	SchedulerError string  `json:"scheduler_error,omitempty"`
}

type TaskMessage struct {
	Seq         int64  `json:"seq"`
	ID          string `json:"message_id"`
	TaskID      string `json:"task_id"`
	Speaker     string `json:"speaker"`
	Content     string `json:"content"`
	RunID       string `json:"run_id,omitempty"`
	Delivery    string `json:"delivery"`
	CreatedAtMS int64  `json:"created_at_ms"`
}

// Artifact content is stored locally in SQLite in this implementation. This
// keeps the online backup atomic across records, decisions and deliverables.
type Artifact struct {
	ID          string `json:"artifact_id"`
	TaskID      string `json:"task_id"`
	RunID       string `json:"run_id"`
	Name        string `json:"name"`
	Content     string `json:"content"`
	Version     int64  `json:"version"`
	SHA256      string `json:"sha256"`
	CreatedAtMS int64  `json:"created_at_ms"`
}

type Review struct {
	DiscussionVersion int64    `json:"discussion_version"`
	ID                string   `json:"review_id"`
	TaskID            string   `json:"task_id"`
	RunID             string   `json:"run_id"`
	State             string   `json:"state"`
	ArtifactIDs       []string `json:"artifact_ids"`
	Comment           string   `json:"comment"`
	CreatedAtMS       int64    `json:"created_at_ms"`
	DecidedAtMS       int64    `json:"decided_at_ms,omitempty"`
}

// TaskSummary is an immutable completion snapshot. Agent-authored qualitative
// notes are kept separate from system-computed timings and counts so later
// efficiency analysis never has to trust a model for operational metrics.
type TaskSummary struct {
	ID                string                 `json:"summary_id"`
	TaskID            string                 `json:"task_id"`
	Version           int64                  `json:"version"`
	SourceType        string                 `json:"source_type"`
	SourceID          string                 `json:"source_id"`
	Title             string                 `json:"title"`
	Goal              string                 `json:"goal"`
	Result            string                 `json:"result"`
	Learnings         []string               `json:"learnings"`
	Improvements      []string               `json:"improvements"`
	Signals           []TaskEfficiencySignal `json:"efficiency_signals"`
	Executor          TaskSummaryExecutor    `json:"executor"`
	AcceptedArtifacts []TaskSummaryArtifact  `json:"accepted_artifacts"`
	Metrics           TaskSummaryMetrics     `json:"metrics"`
	AcceptanceComment string                 `json:"acceptance_comment,omitempty"`
	CompletedAtMS     int64                  `json:"completed_at_ms"`
	CreatedAtMS       int64                  `json:"created_at_ms"`
}

type TaskSummaryExecutor struct {
	AgentID     string `json:"agent_id,omitempty"`
	RuntimeID   string `json:"runtime_id,omitempty"`
	AdapterID   string `json:"adapter_id,omitempty"`
	ModelID     string `json:"model_id,omitempty"`
	RoleID      string `json:"role_id,omitempty"`
	RoleName    string `json:"role_name,omitempty"`
	RoleVersion int64  `json:"role_version,omitempty"`
}

type TaskSummaryArtifact struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	Version    int64  `json:"version"`
	SHA256     string `json:"sha256"`
}

type TaskSummaryMetrics struct {
	CycleTimeMS           int64 `json:"cycle_time_ms"`
	QueueWaitMS           int64 `json:"queue_wait_ms"`
	RuntimeQueueWaitMS    int64 `json:"runtime_queue_wait_ms"`
	ActiveRunTimeMS       int64 `json:"active_run_time_ms"`
	HumanReviewWaitMS     int64 `json:"human_review_wait_ms"`
	RunCount              int   `json:"run_count"`
	CompletedRunCount     int   `json:"completed_run_count"`
	FailedRunCount        int   `json:"failed_run_count"`
	InterruptedRunCount   int   `json:"interrupted_run_count"`
	RunWithoutTimingCount int   `json:"run_without_timing_count"`
	ReviewRoundCount      int   `json:"review_round_count"`
	ChangesRequestedCount int   `json:"changes_requested_count"`
	SupersededReviewCount int   `json:"superseded_review_count"`
	GuidanceMessageCount  int   `json:"guidance_message_count"`
	InterruptCount        int   `json:"interrupt_count"`
	DeliverableCount      int   `json:"deliverable_count"`
	ArtifactVersionCount  int   `json:"artifact_version_count"`
	SubtaskCount          int   `json:"subtask_count"`
	CompletedSubtaskCount int   `json:"completed_subtask_count"`
	BlockedSubtaskCount   int   `json:"blocked_subtask_count"`
}

type TaskEfficiencySignal struct {
	Code       string `json:"code"`
	Evidence   string `json:"evidence"`
	Suggestion string `json:"suggestion"`
}

type WorkDetail struct {
	ReviewTurns []ReviewTurn  `json:"review_turns"`
	Config      WorkConfig    `json:"config"`
	Messages    []TaskMessage `json:"messages"`
	Artifacts   []Artifact    `json:"artifacts"`
	Reviews     []Review      `json:"reviews"`
}
