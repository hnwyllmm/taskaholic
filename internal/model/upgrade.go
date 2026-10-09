package model

// Upgrade is an explicitly authorized change to this application, not a normal
// business task. The installation approval is bound to an immutable candidate.
type Upgrade struct {
	ExecutionSettings
	Builder        *AgentProfile  `json:"builder,omitempty"`
	BuilderBinding *SystemBinding `json:"builder_binding,omitempty"`
	ID             string         `json:"upgrade_id"`
	ChatID         string         `json:"chat_id,omitempty"`
	Title          string         `json:"title"`
	Instructions   string         `json:"instructions"`
	State          string         `json:"state"`
	// RestartScope is derived from the immutable candidate. CONTROL upgrades
	// restart only assistantd; RUNTIME upgrades drain work and restart both
	// assistantd and assistant-runtime. It is never chosen by the build Agent.
	RestartScope     string            `json:"restart_scope,omitempty"`
	Version          int64             `json:"version"`
	BaseRelease      string            `json:"base_release,omitempty"`
	BaseSourceSHA256 string            `json:"base_source_sha256,omitempty"`
	BaseBinaries     map[string]string `json:"base_binaries,omitempty"`
	CandidateSHA256  string            `json:"candidate_sha256,omitempty"`
	SourceSHA256     string            `json:"source_sha256,omitempty"`
	Summary          string            `json:"summary,omitempty"`
	Changes          []UpgradeChange   `json:"changes"`
	Patch            string            `json:"patch,omitempty"`
	Log              string            `json:"log,omitempty"`
	Error            string            `json:"error,omitempty"`
	SessionRef       string            `json:"session_ref,omitempty"`
	BackupDir        string            `json:"backup_dir,omitempty"`
	CreatedAtMS      int64             `json:"created_at_ms"`
	UpdatedAtMS      int64             `json:"updated_at_ms"`
}

const (
	UpgradeRestartControl = "CONTROL"
	UpgradeRestartRuntime = "RUNTIME"
)

type UpgradeChange struct {
	Path   string `json:"path"`
	Before string `json:"before"`
	After  string `json:"after"`
}
