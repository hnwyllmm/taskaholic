package model

import "encoding/json"

const (
	CandidateStateDiscovered = "DISCOVERED"
	CandidateStateValidated  = "VALIDATED"
	CandidateStateCanary     = "CANARY"
	CandidateStatePromoted   = "PROMOTED"
	CandidateStateRejected   = "REJECTED"
	CandidateStatePaused     = "PAUSED"
	CandidateStateRolledBack = "ROLLED_BACK"
	ExperimentStateStable    = "STABLE"

	ExperimentArmBaseline  = "baseline"
	ExperimentArmCandidate = "candidate"
	ExperimentArmPromoted  = "promoted"

	ImprovementSafetyPermissionBoundary     = "permission_boundary"
	ImprovementSafetySecretLeak             = "secret_leak"
	ImprovementSafetyDataDamage             = "data_damage"
	ImprovementSafetyUnauthorizedExternalIO = "unauthorized_external_write"
)

// TaskProfile is deterministic routing and evaluation context. Unknown values
// are explicitly recorded as "other"; an Agent never invents repository or
// authorization scope for this record.
type TaskProfile struct {
	RootTaskID  string `json:"root_task_id"`
	SourceType  string `json:"source_type"`
	TaskType    string `json:"task_type"`
	Repository  string `json:"repository"`
	RoleID      string `json:"role_id"`
	Workflow    string `json:"workflow_type"`
	RuntimeOS   string `json:"runtime_os"`
	ManualAgent bool   `json:"manual_agent"`
}

type ImprovementScope struct {
	SourceType string `json:"source_type,omitempty"`
	TaskType   string `json:"task_type,omitempty"`
	Repository string `json:"repository,omitempty"`
	RoleID     string `json:"role_id,omitempty"`
	Workflow   string `json:"workflow_type,omitempty"`
	RuntimeOS  string `json:"runtime_os,omitempty"`
}

type ExperienceEvidence struct {
	TaskID string `json:"task_id"`
	RunID  string `json:"run_id,omitempty"`
	Review string `json:"review_id,omitempty"`
	Signal string `json:"signal,omitempty"`
}

type Experience struct {
	ID           string               `json:"experience_id"`
	Revision     int64                `json:"revision"`
	Version      int64                `json:"version"`
	Generation   int64                `json:"generation"`
	State        string               `json:"state"`
	Situation    string               `json:"situation"`
	Action       string               `json:"recommended_action"`
	Avoid        string               `json:"avoid"`
	Verification string               `json:"verification"`
	Scope        ImprovementScope     `json:"scope"`
	Evidence     []ExperienceEvidence `json:"evidence"`
	SampleCount  int                  `json:"sample_count"`
	EffectScore  float64              `json:"effect_score"`
	CreatedAtMS  int64                `json:"created_at_ms"`
	UpdatedAtMS  int64                `json:"updated_at_ms"`
}

// ExperiencePatch is the only shape an Analyst may use to propose experience
// catalog changes. Add is the backwards-compatible default. Revisions and
// retirement are canaried against the current immutable revision instead of
// mutating the live catalog when the proposal is created.
type ExperiencePatch struct {
	Operation        string `json:"operation,omitempty"`
	ExperienceID     string `json:"experience_id,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	Situation        string `json:"situation,omitempty"`
	Action           string `json:"recommended_action,omitempty"`
	Avoid            string `json:"avoid,omitempty"`
	Verification     string `json:"verification,omitempty"`
}

type PromptPolicy struct {
	RoleID  string `json:"role_id,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Overlay string `json:"overlay,omitempty"`
}

type RoutingPolicy struct {
	LoadWeight    int `json:"load_weight"`
	SuccessWeight int `json:"success_weight"`
	CostWeight    int `json:"cost_weight"`
}

type RecoveryPolicy struct {
	ContinuousRun         bool  `json:"continuous_run"`
	BackoffMS             int64 `json:"backoff_ms"`
	SplitEnvironmentAfter int   `json:"split_environment_after"`
}

type TestPolicy struct {
	RiskMode      string `json:"risk_mode"`
	ReviewerCount int    `json:"reviewer_count"`
	RetestMode    string `json:"retest_mode"`
}

type WorkflowPolicy struct {
	RequireAgentPlanReview bool `json:"require_agent_plan_review"`
	RequireHumanPlanReview bool `json:"require_human_plan_review"`
	RequirePRReview        bool `json:"require_pr_review"`
	RequirePassingTests    bool `json:"require_passing_tests"`
	RequireHumanAcceptance bool `json:"require_human_acceptance"`
	PlanReviewRoundLimit   int  `json:"plan_review_round_limit"`
}

type OptimizationPolicyHistory struct {
	Event        string `json:"event"`
	State        string `json:"state,omitempty"`
	Reason       string `json:"reason,omitempty"`
	OccurredAtMS int64  `json:"occurred_at_ms"`
}

// OptimizationPolicy is data, not executable code. The immutable security
// kernel is deliberately absent, so a policy patch cannot grant permissions,
// change credentials, weaken backup, or add an arbitrary state transition.
type OptimizationPolicy struct {
	ID          string                      `json:"policy_id"`
	Version     int64                       `json:"version"`
	State       string                      `json:"state"`
	Scope       ImprovementScope            `json:"scope"`
	Prompt      PromptPolicy                `json:"prompt_policy"`
	Routing     RoutingPolicy               `json:"routing_policy"`
	Recovery    RecoveryPolicy              `json:"recovery_policy"`
	Test        TestPolicy                  `json:"test_policy"`
	Workflow    WorkflowPolicy              `json:"workflow_policy"`
	History     []OptimizationPolicyHistory `json:"history,omitempty"`
	CreatedAtMS int64                       `json:"created_at_ms"`
}

type CandidateProposal struct {
	Type      string               `json:"type"`
	Title     string               `json:"title"`
	Rationale string               `json:"rationale"`
	Scope     ImprovementScope     `json:"scope"`
	Patch     json.RawMessage      `json:"patch"`
	Evidence  []ExperienceEvidence `json:"evidence"`
}

type ImprovementCandidate struct {
	ID              string               `json:"candidate_id"`
	Type            string               `json:"type"`
	Title           string               `json:"title"`
	Rationale       string               `json:"rationale"`
	Scope           ImprovementScope     `json:"scope"`
	Patch           json.RawMessage      `json:"patch"`
	Evidence        []ExperienceEvidence `json:"evidence"`
	SourceTaskID    string               `json:"source_task_id"`
	SourceRunID     string               `json:"source_run_id,omitempty"`
	State           string               `json:"state"`
	Version         int64                `json:"version"`
	ValidationError string               `json:"validation_error,omitempty"`
	ExperimentID    string               `json:"experiment_id,omitempty"`
	// LockedFields are operator-owned, scope-specific policy locks. An Analyst
	// cannot populate them through CandidateProposal; only an audited action can.
	LockedFields []string `json:"locked_fields,omitempty"`
	CreatedAtMS  int64    `json:"created_at_ms"`
	UpdatedAtMS  int64    `json:"updated_at_ms"`
}

type ExperimentScore struct {
	QualityChange           float64 `json:"quality_change"`
	HumanInterventionChange float64 `json:"human_intervention_change"`
	CycleTimeChange         float64 `json:"cycle_time_change"`
	TokenChange             float64 `json:"token_change"`
	NoProgressChange        float64 `json:"no_progress_change"`
	Composite               float64 `json:"composite"`
}

type ImprovementExperiment struct {
	ID                     string           `json:"experiment_id"`
	CandidateID            string           `json:"candidate_id"`
	Scope                  ImprovementScope `json:"scope"`
	State                  string           `json:"state"`
	BaselinePolicyID       string           `json:"baseline_policy_id"`
	BaselinePolicyVersion  int64            `json:"baseline_policy_version"`
	CandidatePolicyID      string           `json:"candidate_policy_id,omitempty"`
	CandidatePolicyVersion int64            `json:"candidate_policy_version,omitempty"`
	CandidateExperienceID  string           `json:"candidate_experience_id,omitempty"`
	ExperienceOperation    string           `json:"experience_operation,omitempty"`
	BaselineExperienceRev  int64            `json:"baseline_experience_revision,omitempty"`
	CandidateExperienceRev int64            `json:"candidate_experience_revision,omitempty"`
	BaselineSamples        int              `json:"baseline_samples"`
	CandidateSamples       int              `json:"candidate_samples"`
	PromotedSamples        int              `json:"promoted_samples"`
	Score                  ExperimentScore  `json:"score"`
	DecisionReason         string           `json:"decision_reason,omitempty"`
	CreatedAtMS            int64            `json:"created_at_ms"`
	UpdatedAtMS            int64            `json:"updated_at_ms"`
	PromotedAtMS           int64            `json:"promoted_at_ms,omitempty"`
}

type OptimizationAssignment struct {
	RootTaskID           string      `json:"root_task_id"`
	Profile              TaskProfile `json:"task_profile"`
	ExperimentID         string      `json:"experiment_id,omitempty"`
	Arm                  string      `json:"arm"`
	PolicyID             string      `json:"policy_id"`
	PolicyVersion        int64       `json:"policy_version"`
	ExperienceGeneration int64       `json:"experience_generation"`
	ExperienceIDs        []string    `json:"experience_ids"`
	CreatedAtMS          int64       `json:"created_at_ms"`
}

type SessionExperience struct {
	ExperienceID string `json:"experience_id"`
	Revision     int64  `json:"revision"`
	Rank         int    `json:"rank"`
	Bytes        int    `json:"bytes"`
}

type TaskOptimization struct {
	Assignment         OptimizationAssignment `json:"assignment"`
	SessionExperiences []SessionExperience    `json:"session_experiences"`
}

type ObservationFingerprint struct {
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
	Count       int    `json:"count"`
	Evidence    string `json:"evidence"`
}

type ImprovementObservation struct {
	ID           string                   `json:"observation_id"`
	RootTaskID   string                   `json:"root_task_id"`
	Trigger      string                   `json:"trigger"`
	Profile      TaskProfile              `json:"task_profile"`
	Summary      *TaskSummary             `json:"summary,omitempty"`
	Signals      []TaskEfficiencySignal   `json:"signals"`
	Fingerprints []ObservationFingerprint `json:"fingerprints"`
	TokenUsage   TaskTokenUsage           `json:"token_usage"`
	CreatedAtMS  int64                    `json:"created_at_ms"`
}

type EvaluationEvidence struct {
	Name      string  `json:"name"`
	Available bool    `json:"available"`
	Passed    bool    `json:"passed"`
	Score     float64 `json:"score"`
	Weight    float64 `json:"weight"`
	Reference string  `json:"reference,omitempty"`
}

type TaskEvaluation struct {
	ID                 string               `json:"evaluation_id"`
	TaskID             string               `json:"task_id"`
	RootTaskID         string               `json:"root_task_id"`
	ExperimentID       string               `json:"experiment_id,omitempty"`
	Arm                string               `json:"arm"`
	TaskType           string               `json:"task_type"`
	Mature             bool                 `json:"mature"`
	FailedSample       bool                 `json:"failed_sample"`
	SafetyViolation    bool                 `json:"safety_violation"`
	Coverage           float64              `json:"coverage"`
	QualityScore       float64              `json:"quality_score"`
	HumanInterventions int                  `json:"human_interventions"`
	CycleTimeMS        int64                `json:"cycle_time_ms"`
	TotalTokens        *int64               `json:"total_tokens,omitempty"`
	NoProgressRuns     int                  `json:"no_progress_runs"`
	Evidence           []EvaluationEvidence `json:"evidence"`
	JudgeScore         *float64             `json:"judge_score,omitempty"`
	JudgeReason        string               `json:"judge_reason,omitempty"`
	ObservationDueAtMS int64                `json:"observation_due_at_ms"`
	CreatedAtMS        int64                `json:"created_at_ms"`
	UpdatedAtMS        int64                `json:"updated_at_ms"`
}

type EvaluationArtifact struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	Version    int64  `json:"version"`
	SHA256     string `json:"sha256"`
	Content    string `json:"content,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type EvaluationInput struct {
	TaskID    string               `json:"task_id"`
	TaskType  string               `json:"task_type"`
	Title     string               `json:"title"`
	Goal      string               `json:"goal"`
	Result    string               `json:"result"`
	Artifacts []EvaluationArtifact `json:"artifacts"`
	Evidence  []EvaluationEvidence `json:"evidence"`
}

type SemanticJudgement struct {
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

type ImprovementJob struct {
	ID             string                  `json:"job_id"`
	Kind           string                  `json:"kind"`
	RootTaskID     string                  `json:"root_task_id"`
	State          string                  `json:"state"`
	AvailableAtMS  int64                   `json:"available_at_ms"`
	LeaseOwner     string                  `json:"lease_owner,omitempty"`
	LeaseUntilMS   int64                   `json:"lease_until_ms,omitempty"`
	Attempts       int                     `json:"attempts"`
	InternalTaskID string                  `json:"internal_task_id,omitempty"`
	RunID          string                  `json:"run_id,omitempty"`
	Error          string                  `json:"error,omitempty"`
	Observation    *ImprovementObservation `json:"observation,omitempty"`
	Evaluation     *EvaluationInput        `json:"evaluation,omitempty"`
	CreatedAtMS    int64                   `json:"created_at_ms"`
	UpdatedAtMS    int64                   `json:"updated_at_ms"`
}

type ImprovementReadiness struct {
	ObservationStartedAtMS int64   `json:"observation_started_at_ms"`
	ObservationHours       float64 `json:"observation_hours"`
	DataCoverage           float64 `json:"data_coverage"`
	Ready                  bool    `json:"ready"`
	Reason                 string  `json:"reason"`
}

type ImprovementOverview struct {
	Readiness         ImprovementReadiness `json:"readiness"`
	ActiveExperiments int                  `json:"active_experiments"`
	PendingCandidates int                  `json:"pending_candidates"`
	ExperienceCount   int                  `json:"experience_count"`
	MatureEvaluations int                  `json:"mature_evaluations"`
	AverageQuality    float64              `json:"average_quality"`
	MedianCycleTimeMS int64                `json:"median_cycle_time_ms"`
	MedianHumanInput  int                  `json:"median_human_interventions"`
	BusinessTokens    *int64               `json:"business_tokens,omitempty"`
	ImprovementTokens *int64               `json:"improvement_tokens,omitempty"`
	GrossTokenSaving  *int64               `json:"gross_token_saving,omitempty"`
	NetTokenSaving    *int64               `json:"net_token_saving,omitempty"`
	FailedJobs        int                  `json:"failed_jobs"`
	RecentRollbacks   int                  `json:"recent_rollbacks"`
	ProjectorError    string               `json:"projector_error,omitempty"`
	NeedsAttention    []string             `json:"needs_attention"`
}
