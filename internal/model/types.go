package model

import "encoding/json"

const (
	TaskStateNew          = "NEW"
	TaskStateInProgress   = "IN_PROGRESS"
	TaskStateCompleted    = "COMPLETED"
	TaskStateBlocked      = "BLOCKED"
	TaskStateAssigned     = "ASSIGNED"
	TaskStateWaiting      = "WAITING_SUBTASKS"
	TaskStateWaitingTests = "WAITING_TESTS"

	RunStateQueued      = "QUEUED"
	RunStateRunning     = "RUNNING"
	RunStateCompleted   = "COMPLETED"
	RunStateFailed      = "FAILED"
	RunStateInterrupted = "INTERRUPTED"
)

type Task struct {
	Requirements      TaskRequirements `json:"requirements"`
	ID                string           `json:"task_id"`
	Title             string           `json:"title"`
	Goal              string           `json:"goal"`
	State             string           `json:"state"`
	Version           int64            `json:"version"`
	CurrentRevisionID string           `json:"current_revision_id"`
	AssignedAgentID   string           `json:"assigned_agent_id,omitempty"`
	PreferredAgentID  string           `json:"preferred_agent_id,omitempty"` // Work-list projection, not Session affinity.
	CreatedAtMS       int64            `json:"created_at_ms"`
	UpdatedAtMS       int64            `json:"updated_at_ms"`
}

type TaskRevision struct {
	ID          string `json:"revision_id"`
	TaskID      string `json:"task_id"`
	Number      int64  `json:"revision_number"`
	Goal        string `json:"goal"`
	CreatedAtMS int64  `json:"created_at_ms"`
}

type Run struct {
	ExecutionSettings
	Role         *Role    `json:"role_snapshot,omitempty"`
	Output       string   `json:"output,omitempty"`
	ID           string   `json:"run_id"`
	TaskID       string   `json:"task_id"`
	SessionID    string   `json:"session_id,omitempty"`
	RuntimeID    string   `json:"runtime_id"`
	AgentID      string   `json:"agent_id"`
	AdapterID    string   `json:"adapter_id"`
	ModelID      string   `json:"model_id,omitempty"`
	State        string   `json:"state"`
	Command      []string `json:"command"`
	WorkingDir   string   `json:"working_dir,omitempty"`
	CreatedAtMS  int64    `json:"created_at_ms"`
	StartedAtMS  *int64   `json:"started_at_ms,omitempty"`
	FinishedAtMS *int64   `json:"finished_at_ms,omitempty"`
	ExitCode     *int     `json:"exit_code,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// Session is the durable ownership and affinity record. The control plane
// stores only an opaque agent_session_ref; conversation history and context
// remain owned by the concrete Agent Adapter.
type Session struct {
	ID              string          `json:"session_id"`
	AgentID         string          `json:"agent_id"`
	AdapterID       string          `json:"adapter_id"`
	ModelID         string          `json:"model_id,omitempty"`
	RuntimeID       string          `json:"runtime_id"`
	State           string          `json:"state"`
	AgentSessionRef string          `json:"agent_session_ref,omitempty"`
	MemoryOwner     string          `json:"memory_owner"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
	LastRunID       string          `json:"last_run_id,omitempty"`
	CreatedAtMS     int64           `json:"created_at_ms"`
	UpdatedAtMS     int64           `json:"updated_at_ms"`
}

type TaskEdge struct {
	ID           string `json:"edge_id"`
	FromTaskID   string `json:"from_task_id"`
	ToTaskID     string `json:"to_task_id"`
	Type         string `json:"edge_type"`
	CreatedAtMS  int64  `json:"created_at_ms"`
	CreatedByRun string `json:"created_by_run_id,omitempty"`
}

type SubtaskResult struct {
	Task Task     `json:"task"`
	Edge TaskEdge `json:"edge"`
}

const (
	TaskEdgeDecomposedInto = "DECOMPOSED_INTO"
	// TaskEdgeDelegatedTo is a bounded, Manager-owned light-work fan-out. It
	// participates in the work hierarchy but is distinct from user/API task
	// decomposition so it can be resumed into the originating Agent session.
	TaskEdgeDelegatedTo   = "DELEGATED_TO"

	DirectiveKindMessage   = "message"
	DirectiveKindInterrupt = "interrupt"

	DirectiveStateQueued   = "QUEUED"
	DirectiveStateApplied  = "APPLIED"
	DirectiveStateRejected = "REJECTED"
)

type Directive struct {
	ID          string `json:"directive_id"`
	RunID       string `json:"run_id"`
	TaskID      string `json:"task_id"`
	SessionID   string `json:"session_id,omitempty"`
	Kind        string `json:"kind"`
	Message     string `json:"message,omitempty"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	CreatedAtMS int64  `json:"created_at_ms"`
	AppliedAtMS *int64 `json:"applied_at_ms,omitempty"`
}

type Event struct {
	GlobalSeq     int64           `json:"global_seq"`
	ID            string          `json:"event_id"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	AggregateSeq  int64           `json:"aggregate_seq"`
	Type          string          `json:"event_type"`
	OccurredAtMS  int64           `json:"occurred_at_ms"`
	CausationID   string          `json:"causation_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

type Runtime struct {
	ID             string          `json:"runtime_id"`
	Epoch          string          `json:"epoch"`
	State          string          `json:"state"`
	Hostname       string          `json:"hostname"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	Capabilities   json.RawMessage `json:"capabilities"`
	LastSeenAtMS   int64           `json:"last_seen_at_ms"`
	ConnectedAtMS  int64           `json:"connected_at_ms"`
	DisconnectedAt *int64          `json:"disconnected_at_ms,omitempty"`
}

type RunSpec struct {
	Environment    *EnvironmentExecution `json:"environment,omitempty"`
	ExecutionGrant *ExecutionGrant       `json:"execution_grant,omitempty"`
	// AdditionalWritableRoots is populated only by the runtime after it has
	// created a narrowly scoped local capability such as the VM mailbox. It is
	// never accepted from the control plane.
	AdditionalWritableRoots []string `json:"-"`
	ExecutionSettings
	RequireNativeSession bool            `json:"require_native_session,omitempty"`
	ReadOnly             bool            `json:"read_only,omitempty"`
	Instructions         string          `json:"instructions,omitempty"`
	OutputSchema         json.RawMessage `json:"output_schema,omitempty"`
	RunID                string          `json:"run_id"`
	TaskID               string          `json:"task_id"`
	TaskTitle            string          `json:"task_title,omitempty"`
	TaskGoal             string          `json:"task_goal,omitempty"`
	SessionID            string          `json:"session_id,omitempty"`
	AgentID              string          `json:"agent_id"`
	AdapterID            string          `json:"adapter_id"`
	ModelID              string          `json:"model_id,omitempty"`
	AgentSessionRef      string          `json:"agent_session_ref,omitempty"`
	Command              []string        `json:"command"`
	WorkingDir           string          `json:"working_dir,omitempty"`
	LeaseEpoch           int64           `json:"lease_epoch"`
	LeaseUntil           int64           `json:"lease_until_ms"`
	MessageID            string          `json:"message_id,omitempty"`
}

type RuntimeHello struct {
	RuntimeID     string         `json:"runtime_id"`
	Epoch         string         `json:"epoch"`
	Hostname      string         `json:"hostname"`
	OS            string         `json:"os"`
	Arch          string         `json:"arch"`
	Capabilities  map[string]any `json:"capabilities"`
	LastServerAck int64          `json:"last_server_ack,omitempty"`
}

type RuntimeEvent struct {
	Execution       *ExecutionSettings `json:"execution,omitempty"`
	Activity        *Action            `json:"activity,omitempty"`
	Usage           *TokenUsage        `json:"usage,omitempty"`
	Output          string             `json:"output,omitempty"`
	RuntimeID       string             `json:"runtime_id"`
	Epoch           string             `json:"epoch"`
	RuntimeSeq      int64              `json:"runtime_seq"`
	RunID           string             `json:"run_id"`
	TaskID          string             `json:"task_id"`
	SessionID       string             `json:"session_id,omitempty"`
	DirectiveID     string             `json:"directive_id,omitempty"`
	AgentSessionRef string             `json:"agent_session_ref,omitempty"`
	Type            string             `json:"type"`
	OccurredAt      int64              `json:"occurred_at_ms"`
	Message         string             `json:"message,omitempty"`
	Stream          string             `json:"stream,omitempty"`
	ExitCode        *int               `json:"exit_code,omitempty"`
	Error           string             `json:"error,omitempty"`
	Attributes      map[string]any     `json:"attributes,omitempty"`
	CausationID     string             `json:"causation_id,omitempty"`
}

// TokenUsage is one provider-reported turn snapshot. Cached input and
// reasoning output are subsets of input/output and are not added twice.
type TokenUsage struct {
	Provider              string `json:"provider,omitempty"`
	InputTokens           int64  `json:"input_tokens"`
	CachedInputTokens     int64  `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens int64  `json:"cache_write_input_tokens,omitempty"`
	OutputTokens          int64  `json:"output_tokens"`
	ReasoningOutputTokens int64  `json:"reasoning_output_tokens,omitempty"`
}

type OutboxMessage struct {
	ID              string          `json:"message_id"`
	DestinationID   string          `json:"destination_id"`
	Method          string          `json:"method"`
	Params          json.RawMessage `json:"params"`
	Attempts        int             `json:"attempts"`
	LeaseExpiresMS  int64           `json:"lease_expires_at_ms"`
	NextAttemptAtMS int64           `json:"next_attempt_at_ms"`
}

type TaskDetail struct {
	Task       Task         `json:"task"`
	Summary    *TaskSummary `json:"summary,omitempty"`
	Session    *Session     `json:"session,omitempty"`
	Runs       []Run        `json:"runs"`
	Edges      []TaskEdge   `json:"edges"`
	Directives []Directive  `json:"directives"`
	Events     []Event      `json:"events"`
}

type SessionDetail struct {
	Session Session `json:"session"`
	Tasks   []Task  `json:"tasks"`
	Runs    []Run   `json:"runs"`
}
