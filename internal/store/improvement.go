package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/improvement"
	"work-assistant/internal/model"
)

const (
	defaultPolicyID    = "policy-default"
	maxExperienceCount = 5
	maxExperienceBytes = 6 * 1024
	improvementLease   = 2 * time.Minute
	observationWarmup  = 24 * time.Hour
)

var errPolicyFieldLocked = errors.New("optimization policy field is operator-locked")

func migrateV22(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil || version >= 22 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS task_profile(root_task_id TEXT PRIMARY KEY REFERENCES task(task_id),data_json TEXT NOT NULL,created_at_ms INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS experience(experience_id TEXT PRIMARY KEY,current_revision INTEGER NOT NULL,state TEXT NOT NULL,generation INTEGER NOT NULL,version INTEGER NOT NULL,updated_at_ms INTEGER NOT NULL,data_json TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS experience_scope_idx ON experience(state,generation,updated_at_ms DESC)`,
		`CREATE TABLE IF NOT EXISTS experience_revision(experience_id TEXT NOT NULL REFERENCES experience(experience_id),revision INTEGER NOT NULL,data_json TEXT NOT NULL,created_at_ms INTEGER NOT NULL,PRIMARY KEY(experience_id,revision))`,
		`CREATE TABLE IF NOT EXISTS optimization_policy(policy_id TEXT NOT NULL,version INTEGER NOT NULL,state TEXT NOT NULL,created_at_ms INTEGER NOT NULL,data_json TEXT NOT NULL,PRIMARY KEY(policy_id,version))`,
		`CREATE TABLE IF NOT EXISTS improvement_candidate(candidate_id TEXT PRIMARY KEY,type TEXT NOT NULL,state TEXT NOT NULL,version INTEGER NOT NULL,source_task_id TEXT NOT NULL REFERENCES task(task_id),created_at_ms INTEGER NOT NULL,updated_at_ms INTEGER NOT NULL,fingerprint TEXT NOT NULL UNIQUE,data_json TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS improvement_candidate_state_idx ON improvement_candidate(state,updated_at_ms DESC)`,
		`CREATE TABLE IF NOT EXISTS improvement_experiment(experiment_id TEXT PRIMARY KEY,candidate_id TEXT NOT NULL UNIQUE REFERENCES improvement_candidate(candidate_id),state TEXT NOT NULL,created_at_ms INTEGER NOT NULL,updated_at_ms INTEGER NOT NULL,data_json TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS improvement_experiment_state_idx ON improvement_experiment(state,updated_at_ms DESC)`,
		`CREATE TABLE IF NOT EXISTS experiment_assignment(root_task_id TEXT PRIMARY KEY REFERENCES task(task_id),experiment_id TEXT REFERENCES improvement_experiment(experiment_id),arm TEXT NOT NULL,policy_id TEXT NOT NULL,policy_version INTEGER NOT NULL,experience_generation INTEGER NOT NULL,created_at_ms INTEGER NOT NULL,data_json TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS experiment_assignment_experiment_idx ON experiment_assignment(experiment_id,arm)`,
		`CREATE TABLE IF NOT EXISTS session_experience(session_id TEXT NOT NULL REFERENCES session(session_id),experience_id TEXT NOT NULL REFERENCES experience(experience_id),revision INTEGER NOT NULL,rank INTEGER NOT NULL,bytes INTEGER NOT NULL,PRIMARY KEY(session_id,experience_id))`,
		`CREATE TABLE IF NOT EXISTS task_evaluation(task_id TEXT PRIMARY KEY REFERENCES task(task_id),root_task_id TEXT NOT NULL REFERENCES task(task_id),experiment_id TEXT REFERENCES improvement_experiment(experiment_id),arm TEXT NOT NULL,mature INTEGER NOT NULL,updated_at_ms INTEGER NOT NULL,data_json TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS task_evaluation_experiment_idx ON task_evaluation(experiment_id,arm,mature)`,
		`CREATE TABLE IF NOT EXISTS improvement_job(job_id TEXT PRIMARY KEY,kind TEXT NOT NULL,root_task_id TEXT NOT NULL REFERENCES task(task_id),state TEXT NOT NULL,available_at_ms INTEGER NOT NULL,lease_owner TEXT NOT NULL DEFAULT '',lease_until_ms INTEGER NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0,internal_task_id TEXT REFERENCES task(task_id),run_id TEXT REFERENCES run(run_id),idempotency_key TEXT NOT NULL UNIQUE,data_json TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS improvement_job_due_idx ON improvement_job(state,kind,available_at_ms,lease_until_ms)`,
		`CREATE TABLE IF NOT EXISTS improvement_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err = tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate v22: %w", err)
		}
	}
	now := time.Now().UTC().UnixMilli()
	policy := defaultOptimizationPolicy(now)
	raw, _ := json.Marshal(policy)
	if _, err = tx.Exec(`INSERT OR IGNORE INTO optimization_policy VALUES(?,?,?,?,?)`, policy.ID, policy.Version, policy.State, now, raw); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO improvement_meta VALUES('observation_started_at_ms',?)`, fmt.Sprint(now)); err != nil {
		return err
	}
	// v9 seeded only the then-known system slots. Adding slots here keeps old
	// installations and fresh databases identical.
	for _, slot := range model.SystemSlots {
		var count int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM system_binding WHERE slot=?`, slot.ID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			binding := model.SystemBinding{Slot: slot.ID, Mode: slot.DefaultMode, Version: 1, UpdatedAtMS: now}
			bindingRaw, _ := json.Marshal(binding)
			if _, err = tx.Exec(`INSERT INTO system_binding VALUES(?,?)`, slot.ID, bindingRaw); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(`INSERT INTO schema_version VALUES(22,?)`, now); err != nil {
		return err
	}
	return tx.Commit()
}

func defaultOptimizationPolicy(now int64) model.OptimizationPolicy {
	return model.OptimizationPolicy{
		ID: defaultPolicyID, Version: 1, State: "ACTIVE", CreatedAtMS: now,
		Routing:  model.RoutingPolicy{LoadWeight: 50, SuccessWeight: 30, CostWeight: 20},
		Recovery: model.RecoveryPolicy{ContinuousRun: true, BackoffMS: 2000, SplitEnvironmentAfter: 2},
		Test:     model.TestPolicy{RiskMode: "risk_based", ReviewerCount: 3, RetestMode: "affected"},
		Workflow: model.WorkflowPolicy{RequireAgentPlanReview: true, RequireHumanPlanReview: true, RequirePRReview: true, RequirePassingTests: true, RequireHumanAcceptance: true, PlanReviewRoundLimit: 8},
	}
}

func rootTaskIDTx(ctx context.Context, tx *sql.Tx, taskID string) (string, error) {
	var root string
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE parents(task_id) AS (
		SELECT ? UNION SELECT e.from_task_id FROM task_edge e JOIN parents p ON e.to_task_id=p.task_id
		WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS')
	) SELECT p.task_id FROM parents p WHERE NOT EXISTS(
		SELECT 1 FROM task_edge e WHERE e.to_task_id=p.task_id AND e.edge_type IN ('DECOMPOSED_INTO','REVIEWS')
	) ORDER BY p.task_id LIMIT 1`, taskID).Scan(&root)
	return root, err
}

func ensureTaskProfileTx(ctx context.Context, tx *sql.Tx, taskID string, req CreateRunRequest) (model.TaskProfile, error) {
	root, err := rootTaskIDTx(ctx, tx, taskID)
	if err != nil {
		return model.TaskProfile{}, err
	}
	profile, err := readJSONRow[model.TaskProfile](tx.QueryRowContext(ctx, `SELECT data_json FROM task_profile WHERE root_task_id=?`, root))
	if err == nil {
		return finalizeTaskProfileBeforeFirstRunTx(ctx, tx, profile, req)
	}
	if err != sql.ErrNoRows {
		return profile, err
	}
	var createdAt int64
	if err = tx.QueryRowContext(ctx, `SELECT created_at_ms FROM task WHERE task_id=?`, root).Scan(&createdAt); err != nil {
		return profile, err
	}
	profile = model.TaskProfile{RootTaskID: root, SourceType: "other", TaskType: "other", Repository: "other", RoleID: "other", Workflow: "standard", RuntimeOS: "other"}
	var eventType string
	var payload []byte
	if err = tx.QueryRowContext(ctx, `SELECT event_type,payload_json FROM event_log WHERE aggregate_type='task' AND aggregate_id=? AND event_type IN ('ManualTaskSubmitted','ExternalTaskSubmitted') ORDER BY global_seq LIMIT 1`, root).Scan(&eventType, &payload); err == nil {
		var source struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(payload, &source)
		if eventType == "ManualTaskSubmitted" {
			profile.SourceType = "manual"
		} else if source.Source != "" {
			profile.SourceType = source.Source
		}
	} else if err != sql.ErrNoRows {
		return profile, err
	}
	var hint struct {
		TaskType   string `json:"task_type"`
		Repository string `json:"repository"`
		Workflow   string `json:"workflow_type"`
	}
	_ = tx.QueryRowContext(ctx, `SELECT value FROM improvement_meta WHERE key=?`, "task-hint:"+root).Scan(&payload)
	_ = json.Unmarshal(payload, &hint)
	if hint.TaskType != "" {
		profile.TaskType = hint.TaskType
	}
	if hint.Repository != "" {
		profile.Repository = hint.Repository
	}
	if hint.Workflow != "" {
		profile.Workflow = hint.Workflow
	}
	var developmentRaw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data_json FROM development WHERE task_id=?`, root).Scan(&developmentRaw); err == nil {
		var development model.Development
		if json.Unmarshal(developmentRaw, &development) == nil {
			profile.TaskType = "code"
			if development.Repository != "" {
				profile.Repository = development.Repository
			}
		}
	} else if err != sql.ErrNoRows {
		return profile, err
	}
	if req.AgentID != "" {
		var roleID string
		if err = tx.QueryRowContext(ctx, `SELECT role_id FROM agent_profile WHERE agent_id=?`, req.AgentID).Scan(&roleID); err == nil {
			profile.RoleID = roleID
		} else if err != sql.ErrNoRows {
			return profile, err
		}
	}
	if profile.RoleID == "other" {
		var requirementsRaw []byte
		if err = tx.QueryRowContext(ctx, `SELECT requirements_json FROM task WHERE task_id=?`, root).Scan(&requirementsRaw); err != nil {
			return profile, err
		}
		var requirements model.TaskRequirements
		if json.Unmarshal(requirementsRaw, &requirements) == nil && requirements.RoleID != "" {
			profile.RoleID = requirements.RoleID
		}
	}
	if req.RuntimeID != "" {
		var runtimeOS string
		if scanErr := tx.QueryRowContext(ctx, `SELECT os FROM runtime WHERE runtime_id=?`, req.RuntimeID).Scan(&runtimeOS); scanErr == nil && runtimeOS != "" {
			profile.RuntimeOS = runtimeOS
		}
	}
	var preferred string
	_ = tx.QueryRowContext(ctx, `SELECT agent_id FROM task_workflow WHERE task_id=?`, root).Scan(&preferred)
	profile.ManualAgent = preferred != ""
	raw, _ := json.Marshal(profile)
	if _, err = tx.ExecContext(ctx, `INSERT INTO task_profile VALUES(?,?,?)`, root, raw, time.Now().UnixMilli()); err != nil {
		return profile, err
	}
	_, err = appendEventTx(ctx, tx, "task", root, "TaskProfilePinned", "", root, profile)
	return profile, err
}

func finalizeTaskProfileBeforeFirstRunTx(ctx context.Context, tx *sql.Tx, profile model.TaskProfile, req CreateRunRequest) (model.TaskProfile, error) {
	if req.AgentID == "" && req.RuntimeID == "" {
		return profile, nil
	}
	const scope = `WITH RECURSIVE scope(task_id) AS (SELECT ? UNION SELECT e.to_task_id FROM task_edge e JOIN scope s ON e.from_task_id=s.task_id WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS'))`
	var runs int
	if err := tx.QueryRowContext(ctx, scope+` SELECT COUNT(*) FROM run r JOIN scope s ON s.task_id=r.task_id`, profile.RootTaskID).Scan(&runs); err != nil {
		return profile, err
	}
	if runs > 0 {
		return profile, nil
	}
	updated := profile
	// Development is created only after assignment to a role with the explicit
	// code.implement capability. It is therefore a deterministic workflow fact,
	// not a model guess. A profile prepared while the task was still waiting for
	// routing may be enriched until the first Run is committed.
	if updated.TaskType == "other" {
		var developmentRaw []byte
		if err := tx.QueryRowContext(ctx, `SELECT data_json FROM development WHERE task_id=?`, profile.RootTaskID).Scan(&developmentRaw); err == nil {
			var development model.Development
			if json.Unmarshal(developmentRaw, &development) == nil {
				updated.TaskType = "code"
				if updated.Repository == "other" && development.Repository != "" {
					updated.Repository = development.Repository
				}
			}
		} else if err != sql.ErrNoRows {
			return profile, err
		}
	}
	if updated.RoleID == "other" && req.AgentID != "" {
		var roleID string
		if err := tx.QueryRowContext(ctx, `SELECT role_id FROM agent_profile WHERE agent_id=?`, req.AgentID).Scan(&roleID); err == nil && roleID != "" {
			updated.RoleID = roleID
		} else if err != nil && err != sql.ErrNoRows {
			return profile, err
		}
	}
	if updated.RuntimeOS == "other" && req.RuntimeID != "" {
		var runtimeOS string
		if err := tx.QueryRowContext(ctx, `SELECT os FROM runtime WHERE runtime_id=?`, req.RuntimeID).Scan(&runtimeOS); err == nil && runtimeOS != "" {
			updated.RuntimeOS = runtimeOS
		} else if err != nil && err != sql.ErrNoRows {
			return profile, err
		}
	}
	if updated == profile {
		return profile, nil
	}
	raw, _ := json.Marshal(updated)
	if _, err := tx.ExecContext(ctx, `UPDATE task_profile SET data_json=? WHERE root_task_id=?`, raw, profile.RootTaskID); err != nil {
		return profile, err
	}
	if _, err := appendEventTx(ctx, tx, "task", profile.RootTaskID, "TaskProfileFinalized", "", profile.RootTaskID, map[string]any{"before": profile, "after": updated}); err != nil {
		return profile, err
	}
	return updated, nil
}

func scopeMatches(scope model.ImprovementScope, profile model.TaskProfile) bool {
	checks := [][2]string{{scope.SourceType, profile.SourceType}, {scope.TaskType, profile.TaskType}, {scope.Repository, profile.Repository}, {scope.RoleID, profile.RoleID}, {scope.Workflow, profile.Workflow}, {scope.RuntimeOS, profile.RuntimeOS}}
	for _, check := range checks {
		if check[0] != "" && check[0] != check[1] {
			return false
		}
	}
	return true
}

func ensureOptimizationAssignmentTx(ctx context.Context, tx *sql.Tx, taskID string, req CreateRunRequest) (model.OptimizationAssignment, model.OptimizationPolicy, error) {
	profile, err := ensureTaskProfileTx(ctx, tx, taskID, req)
	if err != nil {
		return model.OptimizationAssignment{}, model.OptimizationPolicy{}, err
	}
	assignment, err := readJSONRow[model.OptimizationAssignment](tx.QueryRowContext(ctx, `SELECT data_json FROM experiment_assignment WHERE root_task_id=?`, profile.RootTaskID))
	if err == nil {
		if assignment.Profile == profile {
			policy, policyErr := getPolicyTx(ctx, tx, assignment.PolicyID, assignment.PolicyVersion)
			return assignment, policy, policyErr
		}
		// PrepareTaskOptimization may have run before the scheduler knew the
		// selected runtime. Re-pin while there is still no Run; after the first
		// Run, finalizeTaskProfileBeforeFirstRunTx never changes the profile.
		if _, err = tx.ExecContext(ctx, `DELETE FROM experiment_assignment WHERE root_task_id=?`, profile.RootTaskID); err != nil {
			return assignment, model.OptimizationPolicy{}, err
		}
		if _, err = appendEventTx(ctx, tx, "task", profile.RootTaskID, "OptimizationAssignmentRepinnedBeforeFirstRun", "", profile.RootTaskID, map[string]any{"previous": assignment, "profile": profile}); err != nil {
			return assignment, model.OptimizationPolicy{}, err
		}
		err = sql.ErrNoRows
	}
	if err != sql.ErrNoRows {
		return assignment, model.OptimizationPolicy{}, err
	}
	now := time.Now().UnixMilli()
	policy, err := activePolicyTx(ctx, tx, profile)
	if err != nil {
		return assignment, policy, err
	}
	assignment = model.OptimizationAssignment{RootTaskID: profile.RootTaskID, Profile: profile, Arm: model.ExperimentArmBaseline, PolicyID: policy.ID, PolicyVersion: policy.Version, CreatedAtMS: now}
	_ = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(json_extract(data_json,'$.generation') AS INTEGER)),0) FROM experience_revision`).Scan(&assignment.ExperienceGeneration)
	var activatedAt int64
	_ = tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM improvement_meta WHERE key='observation_started_at_ms'`).Scan(&activatedAt)
	// Tasks that existed before the feature activation are pinned to baseline.
	var rootCreated int64
	_ = tx.QueryRowContext(ctx, `SELECT created_at_ms FROM task WHERE task_id=?`, profile.RootTaskID).Scan(&rootCreated)
	if rootCreated >= activatedAt {
		experiments, listErr := listJSONRows[model.ImprovementExperiment](ctx, tx, `SELECT data_json FROM improvement_experiment WHERE state IN ('CANARY','PROMOTED') ORDER BY (state='CANARY') DESC,created_at_ms DESC`)
		if listErr != nil {
			return assignment, policy, listErr
		}
		for _, experiment := range experiments {
			// A task that already existed when an experiment was created is not a
			// valid online control/candidate sample. Historical tasks may provide
			// evidence for discovering the candidate, but their behavior was not
			// prospectively assigned and therefore must remain on the baseline.
			if rootCreated < experiment.CreatedAtMS {
				continue
			}
			if !scopeMatches(experiment.Scope, profile) || (experiment.State == "CANARY" && profile.ManualAgent && candidateTypeTx(ctx, tx, experiment.CandidateID) == "routing") {
				continue
			}
			// If the concrete Runtime was unknown during routing, do not assign a
			// routing candidate after an Agent has already been selected under a
			// different policy. Other candidate types are still safe to pin here.
			if req.AgentID != "" && candidateTypeTx(ctx, tx, experiment.CandidateID) == "routing" {
				var applied int
				_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log WHERE aggregate_type='task' AND aggregate_id=? AND event_type='RoutingPolicyRanked' AND json_extract(payload_json,'$.policy_id')=? AND json_extract(payload_json,'$.policy_version')=?`, profile.RootTaskID, experiment.CandidatePolicyID, experiment.CandidatePolicyVersion).Scan(&applied)
				if applied == 0 {
					continue
				}
			}
			assignment.ExperimentID = experiment.ID
			if experiment.State == "PROMOTED" {
				assignment.Arm = model.ExperimentArmPromoted
			} else if stableCandidateArm(profile.RootTaskID, experiment.ID) {
				assignment.Arm = model.ExperimentArmCandidate
			}
			// Both arms compare against the policy snapshot captured when the
			// experiment was created. A later manual policy state change must not
			// silently move the control group or confound an experience experiment.
			assignment.PolicyID, assignment.PolicyVersion = experiment.BaselinePolicyID, experiment.BaselinePolicyVersion
			policy, err = getPolicyTx(ctx, tx, assignment.PolicyID, assignment.PolicyVersion)
			if err != nil {
				return assignment, policy, err
			}
			if assignment.Arm != model.ExperimentArmBaseline && experiment.CandidatePolicyID != "" {
				assignment.PolicyID, assignment.PolicyVersion = experiment.CandidatePolicyID, experiment.CandidatePolicyVersion
				policy, err = getPolicyTx(ctx, tx, assignment.PolicyID, assignment.PolicyVersion)
				if err != nil {
					return assignment, policy, err
				}
			}
			break // exactly one experiment per profile
		}
	}
	raw, _ := json.Marshal(assignment)
	var experimentID any
	if assignment.ExperimentID != "" {
		experimentID = assignment.ExperimentID
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO experiment_assignment VALUES(?,?,?,?,?,?,?,?)`, assignment.RootTaskID, experimentID, assignment.Arm, assignment.PolicyID, assignment.PolicyVersion, assignment.ExperienceGeneration, now, raw); err != nil {
		return assignment, policy, err
	}
	_, err = appendEventTx(ctx, tx, "task", profile.RootTaskID, "OptimizationAssigned", "", profile.RootTaskID, assignment)
	return assignment, policy, err
}

func stableCandidateArm(rootTaskID, experimentID string) bool {
	hash := sha256.Sum256([]byte(rootTaskID + "\x00" + experimentID))
	return hash[0]&1 == 1
}

func candidateTypeTx(ctx context.Context, tx *sql.Tx, id string) string {
	var value string
	_ = tx.QueryRowContext(ctx, `SELECT type FROM improvement_candidate WHERE candidate_id=?`, id).Scan(&value)
	return value
}

func activePolicyTx(ctx context.Context, tx *sql.Tx, profile model.TaskProfile) (model.OptimizationPolicy, error) {
	policies, err := listJSONRows[model.OptimizationPolicy](ctx, tx, `SELECT data_json FROM optimization_policy WHERE state='ACTIVE' ORDER BY version DESC,created_at_ms DESC`)
	if err != nil {
		return model.OptimizationPolicy{}, err
	}
	for _, policy := range policies {
		if scopeMatches(policy.Scope, profile) {
			return policy, nil
		}
	}
	return getPolicyTx(ctx, tx, defaultPolicyID, 1)
}

func getPolicyTx(ctx context.Context, tx *sql.Tx, id string, version int64) (model.OptimizationPolicy, error) {
	return readJSONRow[model.OptimizationPolicy](tx.QueryRowContext(ctx, `SELECT data_json FROM optimization_policy WHERE policy_id=? AND version=?`, id, version))
}

func optimizationPolicyForTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (model.OptimizationPolicy, error) {
	root, err := rootTaskIDTx(ctx, tx, taskID)
	if err != nil {
		return model.OptimizationPolicy{}, err
	}
	assignment, err := readJSONRow[model.OptimizationAssignment](tx.QueryRowContext(ctx, `SELECT data_json FROM experiment_assignment WHERE root_task_id=?`, root))
	if err == sql.ErrNoRows {
		return getPolicyTx(ctx, tx, defaultPolicyID, 1)
	}
	if err != nil {
		return model.OptimizationPolicy{}, err
	}
	return getPolicyTx(ctx, tx, assignment.PolicyID, assignment.PolicyVersion)
}

// PrepareTaskOptimization freezes the root profile, policy, experiment arm,
// and experience catalog before the first Run. It does not create a Session or
// inject memory; that remains part of StartWorkRun for the selected Agent.
func (s *Store) PrepareTaskOptimization(ctx context.Context, taskID string) (model.OptimizationAssignment, model.OptimizationPolicy, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.OptimizationAssignment{}, model.OptimizationPolicy{}, err
	}
	defer tx.Rollback()
	assignment, policy, err := ensureOptimizationAssignmentTx(ctx, tx, taskID, CreateRunRequest{TaskID: taskID})
	if err != nil {
		return assignment, policy, err
	}
	return assignment, policy, tx.Commit()
}

// SelectAgentByOptimizationPolicy ranks only the candidates already filtered
// by the scheduler. The ordinary router and Store assignment validation still
// recheck capacity, role, adapter, runtime, and author exclusions.
func (s *Store) SelectAgentByOptimizationPolicy(ctx context.Context, taskID string, candidates []model.AgentProfile) (string, error) {
	if len(candidates) == 0 {
		return "", fmt.Errorf("%w: no eligible routing candidates", model.ErrConflict)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// If every eligible Agent runs on the same OS, finalize that deterministic
	// profile dimension before selecting the routing policy. Mixed-OS pools use
	// the already pinned baseline; an OS-scoped routing candidate cannot be
	// fairly assigned after selection.
	profileReq := CreateRunRequest{TaskID: taskID}
	var onlyOS string
	var representative model.AgentProfile
	for _, candidate := range candidates {
		var runtimeOS string
		if scanErr := tx.QueryRowContext(ctx, `SELECT os FROM runtime WHERE runtime_id=?`, candidate.RuntimeID).Scan(&runtimeOS); scanErr != nil {
			return "", scanErr
		}
		if onlyOS == "" {
			onlyOS, representative = runtimeOS, candidate
		} else if runtimeOS != onlyOS {
			onlyOS = "mixed"
			break
		}
	}
	if onlyOS != "" && onlyOS != "mixed" {
		profileReq.AgentID, profileReq.RuntimeID = representative.ID, representative.RuntimeID
	}
	_, policy, err := ensureOptimizationAssignmentTx(ctx, tx, taskID, profileReq)
	if err != nil {
		return "", err
	}
	type rankedAgent struct {
		ID      string
		Load    float64
		Success float64
		Tokens  *int64
		Score   float64
	}
	ranked := make([]rankedAgent, 0, len(candidates))
	var observedTokens []int64
	for _, candidate := range candidates {
		if candidate.MaxConcurrent < 1 {
			continue
		}
		item := rankedAgent{ID: candidate.ID, Load: float64(candidate.ActiveRuns) / float64(candidate.MaxConcurrent), Success: .5}
		var samples, successful int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN e.quality_score>=60 THEN 1 ELSE 0 END),0) FROM (SELECT CAST(json_extract(data_json,'$.quality_score') AS REAL) quality_score,task_id FROM task_evaluation WHERE mature=1) e JOIN task_summary s ON s.task_id=e.task_id WHERE json_extract(s.data_json,'$.executor.agent_id')=?`, candidate.ID).Scan(&samples, &successful); err != nil {
			return "", err
		}
		if samples > 0 {
			item.Success = float64(successful) / float64(samples)
		}
		rows, queryErr := tx.QueryContext(ctx, `SELECT CAST(json_extract(e.data_json,'$.total_tokens') AS INTEGER) FROM task_evaluation e JOIN task_summary s ON s.task_id=e.task_id WHERE e.mature=1 AND json_type(e.data_json,'$.total_tokens') IS NOT NULL AND json_extract(s.data_json,'$.executor.agent_id')=? ORDER BY CAST(json_extract(e.data_json,'$.total_tokens') AS INTEGER)`, candidate.ID)
		if queryErr != nil {
			return "", queryErr
		}
		var costs []int64
		for rows.Next() {
			var cost int64
			if queryErr = rows.Scan(&cost); queryErr != nil {
				rows.Close()
				return "", queryErr
			}
			costs = append(costs, cost)
		}
		if queryErr = rows.Close(); queryErr != nil {
			return "", queryErr
		}
		if len(costs) > 0 {
			median := costs[len(costs)/2]
			item.Tokens = &median
			observedTokens = append(observedTokens, median)
		}
		ranked = append(ranked, item)
	}
	if len(ranked) == 0 {
		return "", fmt.Errorf("%w: no eligible routing candidates", model.ErrConflict)
	}
	var minTokens, maxTokens int64
	if len(observedTokens) > 0 {
		sort.Slice(observedTokens, func(i, j int) bool { return observedTokens[i] < observedTokens[j] })
		minTokens, maxTokens = observedTokens[0], observedTokens[len(observedTokens)-1]
	}
	for i := range ranked {
		costScore := .5
		if ranked[i].Tokens != nil {
			costScore = 1
			if maxTokens > minTokens {
				costScore = 1 - float64(*ranked[i].Tokens-minTokens)/float64(maxTokens-minTokens)
			}
		}
		ranked[i].Score = (1-ranked[i].Load)*float64(policy.Routing.LoadWeight) + ranked[i].Success*float64(policy.Routing.SuccessWeight) + costScore*float64(policy.Routing.CostWeight)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].ID < ranked[j].ID
	})
	if _, err = appendEventTx(ctx, tx, "task", taskID, "RoutingPolicyRanked", "", taskID, map[string]any{"policy_id": policy.ID, "policy_version": policy.Version, "selected_agent_id": ranked[0].ID, "score": ranked[0].Score}); err != nil {
		return "", err
	}
	return ranked[0].ID, tx.Commit()
}

func recallExperiencesTx(ctx context.Context, tx *sql.Tx, assignment model.OptimizationAssignment) ([]model.Experience, string, error) {
	// Read immutable revision snapshots, not the mutable catalog head. This is
	// what makes an assignment's catalog generation a real freeze boundary.
	revisions, err := listJSONRows[model.Experience](ctx, tx, `SELECT data_json FROM experience_revision WHERE CAST(json_extract(data_json,'$.generation') AS INTEGER)<=? ORDER BY experience_id,revision DESC`, assignment.ExperienceGeneration)
	if err != nil {
		return nil, "", err
	}
	type revisionKey struct {
		ID       string
		Revision int64
	}
	shadows := map[revisionKey]bool{}
	experiments, err := listJSONRows[model.ImprovementExperiment](ctx, tx, `SELECT data_json FROM improvement_experiment WHERE COALESCE(json_extract(data_json,'$.candidate_experience_id'),'')!=''`)
	if err != nil {
		return nil, "", err
	}
	var assignedExperiment model.ImprovementExperiment
	for _, experiment := range experiments {
		if experiment.CandidateExperienceID != "" && experiment.CandidateExperienceRev > 0 {
			shadows[revisionKey{experiment.CandidateExperienceID, experiment.CandidateExperienceRev}] = true
		}
		if experiment.ID == assignment.ExperimentID {
			assignedExperiment = experiment
		}
	}
	byID := map[string][]model.Experience{}
	for _, revision := range revisions {
		byID[revision.ID] = append(byID[revision.ID], revision)
	}
	items := make([]model.Experience, 0, len(byID))
	for experienceID, history := range byID {
		if assignment.Arm == model.ExperimentArmCandidate && assignedExperiment.CandidateExperienceID == experienceID {
			switch assignedExperiment.ExperienceOperation {
			case "retire":
				continue
			case "add", "revise", "":
				for _, revision := range history {
					if revision.Revision == assignedExperiment.CandidateExperienceRev || assignedExperiment.CandidateExperienceRev == 0 && revision.State == "CANARY" {
						items = append(items, revision)
						break
					}
				}
				continue
			}
		}
		for _, revision := range history {
			if shadows[revisionKey{revision.ID, revision.Revision}] {
				continue // the baseline arm keeps the previous published revision
			}
			if revision.State == "ACTIVE" {
				items = append(items, revision)
			}
			break // a published PAUSED/RETIRED/ROLLED_BACK head disables it
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].EffectScore != items[j].EffectScore {
			return items[i].EffectScore > items[j].EffectScore
		}
		if items[i].SampleCount != items[j].SampleCount {
			return items[i].SampleCount > items[j].SampleCount
		}
		return items[i].UpdatedAtMS > items[j].UpdatedAtMS
	})
	selected := []model.Experience{}
	used := 0
	var prompt strings.Builder
	for _, experience := range items {
		if len(selected) >= maxExperienceCount || !scopeMatches(experience.Scope, assignment.Profile) {
			continue
		}
		entry := fmt.Sprintf("\n经验 %d（版本 %d）\n适用：%s\n推荐：%s\n避免：%s\n验证：%s\n", len(selected)+1, experience.Revision, experience.Situation, experience.Action, experience.Avoid, experience.Verification)
		if used+len(entry) > maxExperienceBytes {
			continue
		}
		used += len(entry)
		selected = append(selected, experience)
		prompt.WriteString(entry)
	}
	if len(selected) == 0 {
		return selected, "", nil
	}
	return selected, "\n\n本 Session 固定注入的已验证团队经验（只影响本 Session，不覆盖任务要求或权限）：" + prompt.String(), nil
}

func persistSessionExperiencesTx(ctx context.Context, tx *sql.Tx, sessionID string, items []model.Experience) ([]model.SessionExperience, error) {
	pinned := make([]model.SessionExperience, 0, len(items))
	for rank, item := range items {
		raw, _ := json.Marshal(item)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_experience VALUES(?,?,?,?,?)`, sessionID, item.ID, item.Revision, rank+1, len(raw)); err != nil {
			return nil, err
		}
		pinned = append(pinned, model.SessionExperience{ExperienceID: item.ID, Revision: item.Revision, Rank: rank + 1, Bytes: len(raw)})
	}
	return pinned, nil
}

func sessionExperienceProfileTx(ctx context.Context, tx *sql.Tx, assignment model.OptimizationAssignment, req CreateRunRequest) (model.TaskProfile, error) {
	profile := assignment.Profile
	if req.AgentID != "" {
		var roleID string
		if err := tx.QueryRowContext(ctx, `SELECT role_id FROM agent_profile WHERE agent_id=?`, req.AgentID).Scan(&roleID); err != nil {
			return profile, err
		} else if roleID != "" {
			profile.RoleID = roleID
		}
	}
	if req.RuntimeID != "" {
		var runtimeOS string
		if err := tx.QueryRowContext(ctx, `SELECT os FROM runtime WHERE runtime_id=?`, req.RuntimeID).Scan(&runtimeOS); err != nil && err != sql.ErrNoRows {
			return profile, err
		} else if runtimeOS != "" {
			profile.RuntimeOS = runtimeOS
		}
	}
	return profile, nil
}

func optimizationRunStageTx(ctx context.Context, tx *sql.Tx, taskID string) (string, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM environment_job WHERE task_id=?)`, taskID).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return "testing", nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM source_review WHERE task_id=?) OR EXISTS(SELECT 1 FROM task_edge WHERE to_task_id=? AND edge_type='REVIEWS')`, taskID, taskID).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return "review", nil
	}
	var phase string
	err := tx.QueryRowContext(ctx, `SELECT json_extract(data_json,'$.phase') FROM development WHERE task_id=?`, taskID).Scan(&phase)
	if err == nil {
		switch phase {
		case "PLANNING":
			return "planning", nil
		case "IMPLEMENTING":
			return "development", nil
		default:
			return "other", nil
		}
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	return "other", nil
}

func optimizationInstructions(policy model.OptimizationPolicy, roleID, stage string) string {
	parts := []string{}
	roleMatches := policy.Prompt.RoleID == "" || policy.Prompt.RoleID == roleID
	stageMatches := policy.Prompt.Stage == "" || policy.Prompt.Stage == stage
	if strings.TrimSpace(policy.Prompt.Overlay) != "" && roleMatches && stageMatches {
		parts = append(parts, "策略提示覆盖：\n"+policy.Prompt.Overlay)
	}
	parts = append(parts, fmt.Sprintf("固定恢复策略：连续工作=%t，退避=%dms，重复环境阻塞 %d 次后请求拆分环境子任务。", policy.Recovery.ContinuousRun, policy.Recovery.BackoffMS, policy.Recovery.SplitEnvironmentAfter))
	parts = append(parts, fmt.Sprintf("固定测试策略：风险模式=%s，Reviewer 数=%d，复测范围=%s。", policy.Test.RiskMode, policy.Test.ReviewerCount, policy.Test.RetestMode))
	parts = append(parts, fmt.Sprintf("固定工作流策略：保留方案互审、人工方案确认、PR Review、测试与最终验收；方案互审最多 %d 轮。状态跳转和执行授权仍由 Manager 校验。", policy.Workflow.PlanReviewRoundLimit))
	return "\n\n本根任务固定的优化策略版本 " + policy.ID + fmt.Sprintf("@%d：\n", policy.Version) + strings.Join(parts, "\n")
}

func (s *Store) GetTaskOptimization(ctx context.Context, taskID string) (model.TaskOptimization, error) {
	var result model.TaskOptimization
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	root, err := rootTaskIDTx(ctx, tx, taskID)
	if err != nil {
		return result, err
	}
	result.Assignment, err = readJSONRow[model.OptimizationAssignment](tx.QueryRowContext(ctx, `SELECT data_json FROM experiment_assignment WHERE root_task_id=?`, root))
	if err != nil {
		return result, err
	}
	var sessionID string
	if err = tx.QueryRowContext(ctx, `SELECT session_id FROM task_session WHERE task_id=? AND unbound_at_ms IS NULL`, taskID).Scan(&sessionID); err == sql.ErrNoRows {
		err = nil
	} else if err != nil {
		return result, err
	}
	if sessionID != "" {
		rows, queryErr := tx.QueryContext(ctx, `SELECT experience_id,revision,rank,bytes FROM session_experience WHERE session_id=? ORDER BY rank`, sessionID)
		if queryErr != nil {
			return result, queryErr
		}
		for rows.Next() {
			var item model.SessionExperience
			if queryErr = rows.Scan(&item.ExperienceID, &item.Revision, &item.Rank, &item.Bytes); queryErr != nil {
				rows.Close()
				return result, queryErr
			}
			result.SessionExperiences = append(result.SessionExperiences, item)
		}
		err = rows.Close()
	}
	return result, err
}

func (s *Store) ListExperiences(ctx context.Context) ([]model.Experience, error) {
	return listJSONRows[model.Experience](ctx, s.db, `SELECT data_json FROM experience ORDER BY updated_at_ms DESC`)
}

func (s *Store) ListPolicies(ctx context.Context) ([]model.OptimizationPolicy, error) {
	items, err := listJSONRows[model.OptimizationPolicy](ctx, s.db, `SELECT data_json FROM optimization_policy ORDER BY created_at_ms DESC,version DESC`)
	if err != nil {
		return nil, err
	}
	for i := range items {
		rows, queryErr := s.db.QueryContext(ctx, `SELECT event_type,occurred_at_ms,payload_json FROM event_log WHERE aggregate_type='optimization_policy' AND aggregate_id=? ORDER BY global_seq`, items[i].ID)
		if queryErr != nil {
			return nil, queryErr
		}
		for rows.Next() {
			var history model.OptimizationPolicyHistory
			var payload []byte
			if queryErr = rows.Scan(&history.Event, &history.OccurredAtMS, &payload); queryErr != nil {
				rows.Close()
				return nil, queryErr
			}
			var detail struct {
				State  string `json:"state"`
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(payload, &detail)
			history.State, history.Reason = detail.State, detail.Reason
			items[i].History = append(items[i].History, history)
		}
		if queryErr = rows.Close(); queryErr != nil {
			return nil, queryErr
		}
	}
	return items, nil
}

func (s *Store) ListImprovementCandidates(ctx context.Context) ([]model.ImprovementCandidate, error) {
	return listJSONRows[model.ImprovementCandidate](ctx, s.db, `SELECT data_json FROM improvement_candidate ORDER BY created_at_ms DESC`)
}

func (s *Store) GetImprovementCandidate(ctx context.Context, candidateID string) (model.ImprovementCandidate, error) {
	return readJSONRow[model.ImprovementCandidate](s.db.QueryRowContext(ctx, `SELECT data_json FROM improvement_candidate WHERE candidate_id=?`, candidateID))
}

func (s *Store) GetImprovementExperiment(ctx context.Context, experimentID string) (model.ImprovementExperiment, error) {
	return readJSONRow[model.ImprovementExperiment](s.db.QueryRowContext(ctx, `SELECT data_json FROM improvement_experiment WHERE experiment_id=?`, experimentID))
}

func (s *Store) ListImprovementExperiments(ctx context.Context) ([]model.ImprovementExperiment, error) {
	return listJSONRows[model.ImprovementExperiment](ctx, s.db, `SELECT data_json FROM improvement_experiment ORDER BY created_at_ms DESC`)
}

func (s *Store) GetTaskEvaluation(ctx context.Context, taskID string) (model.TaskEvaluation, error) {
	root, err := s.rootTaskID(ctx, taskID)
	if err != nil {
		return model.TaskEvaluation{}, err
	}
	return readJSONRow[model.TaskEvaluation](s.db.QueryRowContext(ctx, `SELECT data_json FROM task_evaluation WHERE task_id=?`, root))
}

func (s *Store) rootTaskID(ctx context.Context, taskID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	return rootTaskIDTx(ctx, tx, taskID)
}

var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(token|password|secret|authorization)[=: ]+[^\s]+`),
	regexp.MustCompile(`(?i)https?://[^/@\s]+:[^/@\s]+@`),
}

func redactObservation(value string) string {
	value = strings.TrimSpace(value)
	for _, pattern := range redactPatterns {
		value = pattern.ReplaceAllString(value, "$1=[REDACTED]")
	}
	if len(value) > 500 {
		value = value[:500] + "…"
	}
	return value
}

func observationFingerprint(kind, value string) model.ObservationFingerprint {
	redacted := redactObservation(value)
	normalized := regexp.MustCompile(`\d+`).ReplaceAllString(strings.ToLower(redacted), "#")
	sum := sha256.Sum256([]byte(kind + "\x00" + normalized))
	return model.ObservationFingerprint{Kind: kind, Fingerprint: hex.EncodeToString(sum[:16]), Count: 1, Evidence: redacted}
}

func enqueueImprovementJobTx(ctx context.Context, tx *sql.Tx, kind, rootTaskID, key string, availableAt int64, payload any) error {
	if kind != "ANALYZE" && kind != "JUDGE" {
		return fmt.Errorf("%w: invalid improvement job kind", model.ErrValidation)
	}
	if key == "" {
		return errors.New("improvement job idempotency key required")
	}
	now := time.Now().UnixMilli()
	job := model.ImprovementJob{ID: id.New("improvement_job"), Kind: kind, RootTaskID: rootTaskID, State: "PENDING", AvailableAtMS: availableAt, CreatedAtMS: now, UpdatedAtMS: now}
	switch value := payload.(type) {
	case model.ImprovementObservation:
		job.Observation = &value
	case model.EvaluationInput:
		job.Evaluation = &value
	case nil:
	default:
		return errors.New("unsupported improvement job payload")
	}
	raw, _ := json.Marshal(job)
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO improvement_job(job_id,kind,root_task_id,state,available_at_ms,idempotency_key,data_json) VALUES(?,?,?,?,?,?,?)`, job.ID, kind, rootTaskID, job.State, availableAt, key, raw)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil
	}
	_, err = appendEventTx(ctx, tx, "improvement_job", job.ID, "ImprovementJobQueued", "", rootTaskID, job)
	return err
}

func projectObservationTx(ctx context.Context, tx *sql.Tx, rootTaskID, trigger string) (model.ImprovementObservation, error) {
	profile, err := readJSONRow[model.TaskProfile](tx.QueryRowContext(ctx, `SELECT data_json FROM task_profile WHERE root_task_id=?`, rootTaskID))
	if err != nil {
		return model.ImprovementObservation{}, err
	}
	observation := model.ImprovementObservation{ID: id.New("observation"), RootTaskID: rootTaskID, Trigger: trigger, Profile: profile, Signals: []model.TaskEfficiencySignal{}, Fingerprints: []model.ObservationFingerprint{}, CreatedAtMS: time.Now().UnixMilli()}
	summary, summaryErr := readJSONRow[model.TaskSummary](tx.QueryRowContext(ctx, `SELECT data_json FROM task_summary WHERE task_id=? ORDER BY version DESC LIMIT 1`, rootTaskID))
	if summaryErr == nil {
		observation.Summary = &summary
		observation.Signals = append(observation.Signals, summary.Signals...)
	} else if summaryErr != sql.ErrNoRows {
		return observation, summaryErr
	}
	const scope = `WITH RECURSIVE scope(task_id) AS (SELECT ? UNION SELECT e.to_task_id FROM task_edge e JOIN scope s ON e.from_task_id=s.task_id WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS'))`
	var totalRuns, reported int
	if err = tx.QueryRowContext(ctx, scope+` SELECT COUNT(DISTINCT r.run_id),COUNT(DISTINCT u.run_id),COALESCE(SUM(u.input_tokens),0),COALESCE(SUM(u.cached_input_tokens),0),COALESCE(SUM(u.cache_write_input_tokens),0),COALESCE(SUM(u.output_tokens),0),COALESCE(SUM(u.reasoning_output_tokens),0),COALESCE(SUM(u.input_tokens+u.output_tokens),0) FROM run r JOIN scope s ON s.task_id=r.task_id LEFT JOIN run_token_usage u ON u.run_id=r.run_id`, rootTaskID).Scan(&totalRuns, &reported, &observation.TokenUsage.InputTokens, &observation.TokenUsage.CachedInputTokens, &observation.TokenUsage.CacheWriteInputTokens, &observation.TokenUsage.OutputTokens, &observation.TokenUsage.ReasoningOutputTokens, &observation.TokenUsage.TotalTokens); err != nil {
		return observation, err
	}
	observation.TokenUsage.RunCount, observation.TokenUsage.ReportedRuns = totalRuns, reported
	observation.TokenUsage.UnreportedRuns = totalRuns - reported
	rows, err := tx.QueryContext(ctx, scope+` SELECT r.error,COUNT(*) FROM run r JOIN scope s ON s.task_id=r.task_id WHERE r.state='FAILED' AND COALESCE(r.error,'')!='' GROUP BY r.error`, rootTaskID)
	if err != nil {
		return observation, err
	}
	fingerprints := map[string]model.ObservationFingerprint{}
	mergeFingerprint := func(kind, value string, count int) {
		fp := observationFingerprint(kind, value)
		if existing, ok := fingerprints[fp.Fingerprint]; ok {
			existing.Count += count
			fingerprints[fp.Fingerprint] = existing
		} else {
			fp.Count = count
			fingerprints[fp.Fingerprint] = fp
		}
	}
	for rows.Next() {
		var message string
		var count int
		if err = rows.Scan(&message, &count); err != nil {
			rows.Close()
			return observation, err
		}
		mergeFingerprint("run_failure", message, count)
	}
	if err = rows.Close(); err != nil {
		return observation, err
	}
	rows, err = tx.QueryContext(ctx, scope+` SELECT json_extract(a.data_json,'$.command'),COUNT(*) FROM run_activity a JOIN scope s ON s.task_id=a.task_id WHERE COALESCE(json_extract(a.data_json,'$.command'),'')!='' GROUP BY json_extract(a.data_json,'$.command') ORDER BY COUNT(*) DESC LIMIT 20`, rootTaskID)
	if err != nil {
		return observation, err
	}
	for rows.Next() {
		var command string
		var count int
		if err = rows.Scan(&command, &count); err != nil {
			rows.Close()
			return observation, err
		}
		mergeFingerprint("command", command, count)
	}
	if err = rows.Close(); err != nil {
		return observation, err
	}
	rows, err = tx.QueryContext(ctx, scope+` SELECT m.content,COUNT(*) FROM task_message m JOIN scope s ON s.task_id=m.task_id WHERE m.speaker='user' GROUP BY m.content ORDER BY COUNT(*) DESC LIMIT 20`, rootTaskID)
	if err != nil {
		return observation, err
	}
	for rows.Next() {
		var feedback string
		var count int
		if err = rows.Scan(&feedback, &count); err != nil {
			rows.Close()
			return observation, err
		}
		mergeFingerprint("human_feedback", feedback, count)
	}
	if err = rows.Close(); err != nil {
		return observation, err
	}
	for _, fp := range fingerprints {
		observation.Fingerprints = append(observation.Fingerprints, fp)
	}
	sort.Slice(observation.Fingerprints, func(i, j int) bool { return observation.Fingerprints[i].Count > observation.Fingerprints[j].Count })
	var noProgress, permissionRequests, environmentBlocks, stageRollbacks, repeatedRecovery, redundantTests int
	var queueWaitMS, activeRunMS int64
	var humanRedirections int
	if err = tx.QueryRowContext(ctx, scope+` SELECT COUNT(*) FROM run r JOIN scope s ON s.task_id=r.task_id WHERE r.state='COMPLETED' AND length(trim(COALESCE(r.output,'')))=0`, rootTaskID).Scan(&noProgress); err != nil {
		return observation, err
	}
	if err = tx.QueryRowContext(ctx, scope+` SELECT COUNT(*) FROM permission_request p JOIN scope s ON s.task_id=p.task_id`, rootTaskID).Scan(&permissionRequests); err != nil {
		return observation, err
	}
	if err = tx.QueryRowContext(ctx, scope+` SELECT COUNT(*) FROM environment_job j JOIN scope s ON s.task_id=j.parent_task_id WHERE j.state NOT IN ('COMPLETED','SUPERSEDED')`, rootTaskID).Scan(&environmentBlocks); err != nil {
		return observation, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log WHERE correlation_id=? AND event_type='DevelopmentRestarted'`, rootTaskID).Scan(&stageRollbacks); err != nil {
		return observation, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log WHERE aggregate_type='task' AND aggregate_id=? AND event_type='TaskBlockedObservation'`, rootTaskID).Scan(&repeatedRecovery); err != nil {
		return observation, err
	}
	if err = tx.QueryRowContext(ctx, scope+` SELECT COALESCE(SUM(n-1),0) FROM (SELECT COUNT(*) n FROM test_pipeline p JOIN scope s ON s.task_id=p.task_id WHERE p.state='success' GROUP BY p.pr_target_id,p.head_sha HAVING COUNT(*)>1)`, rootTaskID).Scan(&redundantTests); err != nil {
		return observation, err
	}
	if err = tx.QueryRowContext(ctx, scope+` SELECT
		COALESCE(SUM(COALESCE(CAST(json_extract(summary.data_json,'$.metrics.queue_wait_ms') AS INTEGER),0)+COALESCE(CAST(json_extract(summary.data_json,'$.metrics.runtime_queue_wait_ms') AS INTEGER),0)),0),
		COALESCE(SUM(COALESCE(CAST(json_extract(summary.data_json,'$.metrics.active_run_time_ms') AS INTEGER),0)),0)
		FROM task_summary summary JOIN scope s ON s.task_id=summary.task_id
		WHERE summary.version=(SELECT MAX(latest.version) FROM task_summary latest WHERE latest.task_id=summary.task_id)`, rootTaskID).Scan(&queueWaitMS, &activeRunMS); err != nil {
		return observation, err
	}
	if err = tx.QueryRowContext(ctx, scope+` SELECT
		COALESCE((SELECT SUM(CASE WHEN message_count>0 THEN message_count-1 ELSE 0 END) FROM (
			SELECT m.task_id,COUNT(*) message_count FROM task_message m JOIN scope s ON s.task_id=m.task_id WHERE m.speaker='user' GROUP BY m.task_id
		)),0)
		+ (SELECT COUNT(*) FROM review r JOIN scope s ON s.task_id=r.task_id WHERE r.state='CHANGES_REQUESTED')
		+ (SELECT COUNT(*) FROM event_log event JOIN scope s ON s.task_id=event.aggregate_id WHERE event.aggregate_type='task' AND event.event_type='ExecutionPermissionDecided')`, rootTaskID).Scan(&humanRedirections); err != nil {
		return observation, err
	}
	if noProgress > 0 {
		observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "no_progress_run", Evidence: fmt.Sprintf("%d 次完成 Run 没有可用结果", noProgress), Suggestion: "减少只重复上下文而没有形成新证据或交付物的续跑。"})
	}
	if permissionRequests > 1 {
		observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "repeated_authorization", Evidence: fmt.Sprintf("任务树包含 %d 次执行授权申请", permissionRequests), Suggestion: "在固定安全边界内复用已批准的精确权限范围。"})
	}
	if environmentBlocks > 0 {
		observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "environment_block", Evidence: fmt.Sprintf("有 %d 个环境任务尚未完成", environmentBlocks), Suggestion: "把可重复的环境修复拆成独立子任务并记录验证方法。"})
	}
	if stageRollbacks > 0 {
		observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "stage_rollback", Evidence: fmt.Sprintf("开发阶段被明确重置 %d 次", stageRollbacks), Suggestion: "识别真正改变已审批方案的输入，只对受影响的阶段和验证范围重新执行。"})
	}
	if repeatedRecovery >= 3 {
		observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "repeated_recovery", Evidence: fmt.Sprintf("任务累计进入受阻恢复路径 %d 次", repeatedRecovery), Suggestion: "优先复用已验证恢复路径，必要时拆分环境修复子任务。"})
	}
	if redundantTests > 0 {
		observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "over_testing", Evidence: fmt.Sprintf("同一 PR commit 重复执行并通过测试 %d 次", redundantTests), Suggestion: "复用同一 commit 的有效测试证据，只有代码、依赖、制品、配置或关键环境变化时才重跑受影响范围。"})
	}
	if queueWaitMS > 60_000 && queueWaitMS > activeRunMS {
		observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "queue_bottleneck", Evidence: fmt.Sprintf("任务树累计排队 %d ms，超过 Agent 实际运行 %d ms", queueWaitMS, activeRunMS), Suggestion: "检查 Agent 在线率、容量、路由匹配和 Runtime 队列。"})
	}
	if humanRedirections > 0 {
		observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "human_redirection", Evidence: fmt.Sprintf("任务树包含 %d 次人工改向或打回", humanRedirections), Suggestion: "把重复的人工作业约束沉淀为角色检查项或适用经验。"})
	}
	for _, fp := range observation.Fingerprints {
		switch {
		case fp.Kind == "run_failure" && fp.Count >= 2:
			observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "repeated_failure", Evidence: fmt.Sprintf("相同脱敏错误指纹重复 %d 次：%s", fp.Count, fp.Fingerprint), Suggestion: "复用已经验证的恢复路径，不重复猜测命令。"})
		case fp.Kind == "command" && fp.Count >= 3:
			observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "repeated_command", Evidence: fmt.Sprintf("相同脱敏命令指纹出现 %d 次：%s", fp.Count, fp.Fingerprint), Suggestion: "检查这些执行是否产生了新证据；相同命令本身不等于失败。"})
		case fp.Kind == "human_feedback" && fp.Count >= 2:
			observation.Signals = append(observation.Signals, model.TaskEfficiencySignal{Code: "repeated_feedback", Evidence: fmt.Sprintf("相同脱敏人工反馈出现 %d 次：%s", fp.Count, fp.Fingerprint), Suggestion: "分析为何已给出的约束没有在后续 Run 中被稳定遵守。"})
		}
	}
	return observation, nil
}

// enqueueCompletionImprovementTx is called in the same transaction that
// creates the immutable TaskSummary. The business task never waits for it.
func enqueueCompletionImprovementTx(ctx context.Context, tx *sql.Tx, taskID string, summary model.TaskSummary) error {
	root, err := rootTaskIDTx(ctx, tx, taskID)
	if err != nil || root != taskID {
		return err
	}
	if _, err = readJSONRow[model.OptimizationAssignment](tx.QueryRowContext(ctx, `SELECT data_json FROM experiment_assignment WHERE root_task_id=?`, root)); err != nil {
		if err != sql.ErrNoRows {
			return err
		}
		if _, _, err = ensureOptimizationAssignmentTx(ctx, tx, root, CreateRunRequest{TaskID: root}); err != nil {
			return err
		}
	}
	observation, err := projectObservationTx(ctx, tx, root, "completed")
	if err != nil {
		return err
	}
	if err = enqueueImprovementJobTx(ctx, tx, "ANALYZE", root, "analyze:"+summary.ID, time.Now().UnixMilli(), observation); err != nil {
		return err
	}
	delay := 24 * time.Hour
	if observation.Profile.TaskType == "code" || observation.Profile.TaskType == "bug" {
		delay = 72 * time.Hour
	}
	input := evaluationInputFromSummary(summary)
	return enqueueImprovementJobTx(ctx, tx, "JUDGE", root, "judge:"+summary.ID, time.Now().Add(delay).UnixMilli(), input)
}

func recordImprovementProjectionFailureTx(ctx context.Context, tx *sql.Tx, taskID, trigger string, cause error) {
	value := map[string]any{"task_id": taskID, "trigger": trigger, "error": redactObservation(cause.Error()), "occurred_at_ms": time.Now().UnixMilli()}
	raw, _ := json.Marshal(value)
	_, _ = tx.ExecContext(ctx, `INSERT OR REPLACE INTO improvement_meta(key,value) VALUES('projector:last_error',?)`, raw)
	_, _ = appendEventTx(ctx, tx, "improvement_projector", taskID, "ImprovementProjectionFailed", "", taskID, value)
}

func tryEnqueueCompletionImprovementTx(ctx context.Context, tx *sql.Tx, taskID string, summary model.TaskSummary) {
	if err := enqueueCompletionImprovementTx(ctx, tx, taskID, summary); err != nil {
		recordImprovementProjectionFailureTx(ctx, tx, taskID, "completed", err)
	}
}

func tryEnqueueIntermediateImprovementTx(ctx context.Context, tx *sql.Tx, taskID, trigger, key string) {
	root, err := rootTaskIDTx(ctx, tx, taskID)
	if err == nil {
		var assigned int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experiment_assignment WHERE root_task_id=?`, root).Scan(&assigned)
		if err == nil && assigned == 0 {
			_, _, err = ensureOptimizationAssignmentTx(ctx, tx, root, CreateRunRequest{TaskID: root})
		}
		if err == nil {
			var observation model.ImprovementObservation
			observation, err = projectObservationTx(ctx, tx, root, trigger)
			if err == nil {
				err = enqueueImprovementJobTx(ctx, tx, "ANALYZE", root, key, time.Now().UnixMilli(), observation)
			}
		}
	}
	if err != nil {
		recordImprovementProjectionFailureTx(ctx, tx, taskID, trigger, err)
	}
}

// ReconcileImprovementJobs projects durable completion facts after a restart
// or a transient improvement-plane failure. Business completion never depends
// on this queue being available in the same transaction.
func (s *Store) ReconcileImprovementJobs(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT summary.data_json
		FROM task_summary summary
		WHERE NOT EXISTS(SELECT 1 FROM task_edge edge WHERE edge.to_task_id=summary.task_id AND edge.edge_type IN ('DECOMPOSED_INTO','REVIEWS'))
		AND NOT EXISTS(SELECT 1 FROM improvement_job job WHERE job.idempotency_key='analyze:'||summary.summary_id)
		ORDER BY summary.completed_at_ms LIMIT 100`)
	if err != nil {
		return err
	}
	var summaries []model.TaskSummary
	for rows.Next() {
		var summary model.TaskSummary
		if summary, err = readJSONRow[model.TaskSummary](rows); err != nil {
			rows.Close()
			return err
		}
		summaries = append(summaries, summary)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	hadFailure := false
	for _, summary := range summaries {
		if err = enqueueCompletionImprovementTx(ctx, tx, summary.TaskID, summary); err != nil {
			hadFailure = true
			recordImprovementProjectionFailureTx(ctx, tx, summary.TaskID, "reconcile_completed", err)
		}
	}
	if !hadFailure {
		if _, err = tx.ExecContext(ctx, `DELETE FROM improvement_meta WHERE key='projector:last_error'`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func enqueueBlockedImprovementTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	root, err := rootTaskIDTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	var assigned int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experiment_assignment WHERE root_task_id=?`, root).Scan(&assigned); err != nil || assigned == 0 {
		return err
	}
	reason := "task entered BLOCKED without a recorded reason"
	_ = tx.QueryRowContext(ctx, `SELECT scheduler_error FROM task_workflow WHERE task_id=? AND trim(scheduler_error)!=''`, taskID).Scan(&reason)
	if reason == "task entered BLOCKED without a recorded reason" {
		var runError, output string
		if scanErr := tx.QueryRowContext(ctx, `SELECT COALESCE(error,''),COALESCE(output,'') FROM run WHERE task_id=? ORDER BY created_at_ms DESC LIMIT 1`, taskID).Scan(&runError, &output); scanErr == nil {
			if strings.TrimSpace(runError) != "" {
				reason = runError
			} else if strings.TrimSpace(output) != "" {
				reason = output
			}
		} else if scanErr != sql.ErrNoRows {
			return scanErr
		}
	}
	blockedFingerprint := observationFingerprint("blocked", reason)
	var previousSame int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log WHERE aggregate_type='task' AND aggregate_id=? AND event_type='TaskBlockedObservation' AND json_extract(payload_json,'$.fingerprint')=?`, root, blockedFingerprint.Fingerprint).Scan(&previousSame); err != nil {
		return err
	}
	observation, err := projectObservationTx(ctx, tx, root, "blocked")
	if err != nil {
		return err
	}
	currentSame := previousSame + 1
	if currentSame >= 3 {
		if err = recordFailedEvaluationTx(ctx, tx, root, "相同阻塞指纹重复至少三次"); err != nil {
			return err
		}
	}
	// Analyze the first occurrence, then every third matching occurrence. A
	// repeated command or repeated human sentence remains a diagnostic signal,
	// but can never manufacture a repeated-block failure sample.
	if currentSame == 1 || currentSame%3 == 0 {
		key := fmt.Sprintf("blocked:%s:%s:%d", root, blockedFingerprint.Fingerprint, currentSame)
		if err = enqueueImprovementJobTx(ctx, tx, "ANALYZE", root, key, time.Now().UnixMilli(), observation); err != nil {
			return err
		}
	}
	_, err = appendEventTx(ctx, tx, "task", root, "TaskBlockedObservation", "", root, map[string]any{"task_id": taskID, "fingerprint": blockedFingerprint.Fingerprint, "occurrence": currentSame, "evidence": blockedFingerprint.Evidence})
	return err
}

// EvaluateBlockedExperimentSamples turns a sustained real BLOCKED state into
// a failure sample. WAITING_AUTHORIZATION / WAITING_USER / WAITING_ENVIRONMENT
// are deliberately different states and are never failed by this sweep.
func (s *Store) EvaluateBlockedExperimentSamples(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE scope(root_task_id,task_id) AS (
		SELECT a.root_task_id,a.root_task_id FROM experiment_assignment a WHERE a.experiment_id IS NOT NULL
		UNION
		SELECT s.root_task_id,e.to_task_id FROM scope s JOIN task_edge e ON e.from_task_id=s.task_id
		WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS')
	)
	SELECT DISTINCT s.root_task_id FROM scope s JOIN task t ON t.task_id=s.task_id
	WHERE t.state='BLOCKED' AND t.updated_at_ms<=?`, time.Now().Add(-24*time.Hour).UnixMilli())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var taskID string
		if err = rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, taskID)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, taskID := range ids {
		if err = recordFailedEvaluationTx(ctx, tx, taskID, "任务持续受阻至少 24 小时"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EvaluateExperimentDeadlines makes the 30-day canary limit independent of
// new task completions. Without this sweep, an idle or low-volume experiment
// could remain active forever simply because no later evaluation arrived to
// call evaluateExperimentTx.
func (s *Store) EvaluateExperimentDeadlines(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT experiment_id FROM improvement_experiment WHERE state='CANARY' AND created_at_ms<=? ORDER BY created_at_ms`, time.Now().Add(-30*24*time.Hour).UnixMilli())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var experimentID string
		if err = rows.Scan(&experimentID); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, experimentID)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, experimentID := range ids {
		if err = evaluateExperimentTx(ctx, tx, experimentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func recordFailedEvaluationTx(ctx context.Context, tx *sql.Tx, rootTaskID, reason string) error {
	if existing, err := readJSONRow[model.TaskEvaluation](tx.QueryRowContext(ctx, `SELECT data_json FROM task_evaluation WHERE task_id=?`, rootTaskID)); err == nil && existing.Mature && existing.FailedSample {
		return nil
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}
	assignment, err := readJSONRow[model.OptimizationAssignment](tx.QueryRowContext(ctx, `SELECT data_json FROM experiment_assignment WHERE root_task_id=?`, rootTaskID))
	if err != nil || assignment.ExperimentID == "" {
		return err
	}
	var createdAt int64
	if err = tx.QueryRowContext(ctx, `SELECT created_at_ms FROM task WHERE task_id=?`, rootTaskID).Scan(&createdAt); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	evaluation := model.TaskEvaluation{
		ID: id.New("evaluation"), TaskID: rootTaskID, RootTaskID: rootTaskID,
		ExperimentID: assignment.ExperimentID, Arm: assignment.Arm, TaskType: assignment.Profile.TaskType,
		Mature: true, FailedSample: true, QualityScore: 0, CycleTimeMS: now - createdAt,
		Evidence:           []model.EvaluationEvidence{{Name: "terminal_failure", Available: true, Passed: false, Score: 0, Weight: 0, Reference: reason}},
		ObservationDueAtMS: now, CreatedAtMS: now, UpdatedAtMS: now,
	}
	const scope = `WITH RECURSIVE scope(task_id) AS (SELECT ? UNION SELECT e.to_task_id FROM task_edge e JOIN scope s ON e.from_task_id=s.task_id WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS'))`
	if err = tx.QueryRowContext(ctx, scope+` SELECT COUNT(*) FROM run r JOIN scope s ON s.task_id=r.task_id WHERE r.state='FAILED' OR (r.state='COMPLETED' AND length(trim(COALESCE(r.output,'')))=0)`, rootTaskID).Scan(&evaluation.NoProgressRuns); err != nil {
		return err
	}
	var runCount, reported int
	var totalTokens int64
	if err = tx.QueryRowContext(ctx, scope+` SELECT COUNT(DISTINCT r.run_id),COUNT(DISTINCT u.run_id),COALESCE(SUM(u.input_tokens+u.output_tokens),0) FROM run r JOIN scope s ON s.task_id=r.task_id LEFT JOIN run_token_usage u ON u.run_id=r.run_id`, rootTaskID).Scan(&runCount, &reported, &totalTokens); err != nil {
		return err
	}
	if runCount > 0 && runCount == reported {
		evaluation.TotalTokens = &totalTokens
	}
	if err = saveTaskEvaluationTx(ctx, tx, evaluation, "TaskEvaluationFailedSample"); err != nil {
		return err
	}
	return evaluateExperimentTx(ctx, tx, assignment.ExperimentID)
}

// RecordImprovementSafetyViolation is the fail-closed bridge from immutable
// permission, credential, data-protection, and publication guards into an
// experiment. Callers report only a guard-proven fact; an Analyst or Judge can
// never invoke this path through model output. Replays are idempotent.
func (s *Store) RecordImprovementSafetyViolation(ctx context.Context, taskID, violation, reference, key string) error {
	allowed := map[string]bool{
		model.ImprovementSafetyPermissionBoundary:     true,
		model.ImprovementSafetySecretLeak:             true,
		model.ImprovementSafetyDataDamage:             true,
		model.ImprovementSafetyUnauthorizedExternalIO: true,
	}
	if !allowed[violation] || strings.TrimSpace(reference) == "" || strings.TrimSpace(key) == "" || len(key) > 240 {
		return fmt.Errorf("%w: safety violation, evidence reference, and idempotency key are required", model.ErrValidation)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	root, err := rootTaskIDTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	scope := "improvement.safety:" + root
	var existing string
	if err = tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope=? AND key=?`, scope, key).Scan(&existing); err == nil {
		return nil
	} else if err != sql.ErrNoRows {
		return err
	}
	assignment, err := readJSONRow[model.OptimizationAssignment](tx.QueryRowContext(ctx, `SELECT data_json FROM experiment_assignment WHERE root_task_id=?`, root))
	if err != nil {
		return err
	}
	redacted := redactObservation(reference)
	if _, err = appendEventTx(ctx, tx, "task", root, "ImprovementSafetyViolationObserved", key, root, map[string]any{"kind": violation, "reference": redacted, "arm": assignment.Arm, "experiment_id": assignment.ExperimentID}); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_key VALUES(?,?,?,?)`, scope, key, root, time.Now().UnixMilli()); err != nil {
		return err
	}
	// A baseline incident is still audited, but cannot be attributed to or used
	// to roll back the candidate under test.
	if assignment.ExperimentID == "" || assignment.Arm == model.ExperimentArmBaseline {
		return tx.Commit()
	}
	now := time.Now().UnixMilli()
	evaluation, readErr := readJSONRow[model.TaskEvaluation](tx.QueryRowContext(ctx, `SELECT data_json FROM task_evaluation WHERE task_id=?`, root))
	if readErr == sql.ErrNoRows {
		var createdAt int64
		if err = tx.QueryRowContext(ctx, `SELECT created_at_ms FROM task WHERE task_id=?`, root).Scan(&createdAt); err != nil {
			return err
		}
		evaluation = model.TaskEvaluation{ID: id.New("evaluation"), TaskID: root, RootTaskID: root, ExperimentID: assignment.ExperimentID, Arm: assignment.Arm, TaskType: assignment.Profile.TaskType, CreatedAtMS: now, CycleTimeMS: now - createdAt}
	} else if readErr != nil {
		return readErr
	}
	evaluation.ExperimentID = assignment.ExperimentID
	evaluation.Arm = assignment.Arm
	evaluation.Mature = true
	evaluation.FailedSample = true
	evaluation.SafetyViolation = true
	evaluation.QualityScore = 0
	evaluation.ObservationDueAtMS = now
	evaluation.Evidence = append(evaluation.Evidence, model.EvaluationEvidence{Name: "safety:" + violation, Available: true, Passed: false, Weight: 0, Reference: redacted})
	if err = saveTaskEvaluationTx(ctx, tx, evaluation, "TaskEvaluationSafetyViolation"); err != nil {
		return err
	}
	if err = evaluateExperimentTx(ctx, tx, assignment.ExperimentID); err != nil {
		return err
	}
	return tx.Commit()
}

func evaluationInputFromSummary(summary model.TaskSummary) model.EvaluationInput {
	taskType := "other"
	artifacts := make([]model.EvaluationArtifact, 0, len(summary.AcceptedArtifacts))
	for _, artifact := range summary.AcceptedArtifacts {
		artifacts = append(artifacts, model.EvaluationArtifact{ArtifactID: artifact.ArtifactID, Name: artifact.Name, Version: artifact.Version, SHA256: artifact.SHA256})
	}
	return model.EvaluationInput{TaskID: summary.TaskID, TaskType: taskType, Title: summary.Title, Goal: summary.Goal, Result: summary.Result, Artifacts: artifacts, Evidence: []model.EvaluationEvidence{}}
}

func (s *Store) ClaimImprovementJob(ctx context.Context, owner string, kinds ...string) (*model.ImprovementJob, error) {
	if owner == "" {
		return nil, errors.New("lease owner required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if len(kinds) > 0 && kinds[0] != "" {
		var active int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM improvement_job WHERE kind=? AND (state IN ('STARTING','RUNNING') OR (state='LEASED' AND lease_until_ms>=?))`, kinds[0], now).Scan(&active); err != nil {
			return nil, err
		}
		if active > 0 {
			return nil, sql.ErrNoRows
		}
	}
	query := `SELECT data_json FROM improvement_job WHERE available_at_ms<=? AND (state='PENDING' OR (state='LEASED' AND lease_until_ms<?))`
	args := []any{now, now}
	if len(kinds) > 0 && kinds[0] != "" {
		query += ` AND kind=?`
		args = append(args, kinds[0])
	}
	query += ` ORDER BY available_at_ms LIMIT 1`
	job, err := readJSONRow[model.ImprovementJob](tx.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, err
	}
	if job.Kind == "JUDGE" {
		evaluation, input, prepareErr := prepareEvaluationTx(ctx, tx, job.RootTaskID)
		if prepareErr != nil {
			return nil, prepareErr
		}
		job.Evaluation = &input
		if prepareErr = saveTaskEvaluationTx(ctx, tx, evaluation, "TaskEvaluationPrepared"); prepareErr != nil {
			return nil, prepareErr
		}
	}
	job.State, job.LeaseOwner, job.LeaseUntilMS, job.UpdatedAtMS = "LEASED", owner, now+improvementLease.Milliseconds(), now
	raw, _ := json.Marshal(job)
	result, err := tx.ExecContext(ctx, `UPDATE improvement_job SET state='LEASED',lease_owner=?,lease_until_ms=?,data_json=? WHERE job_id=? AND (state='PENDING' OR lease_until_ms<?)`, owner, job.LeaseUntilMS, raw, job.ID, now)
	if err != nil {
		return nil, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	if _, err = appendEventTx(ctx, tx, "improvement_job", job.ID, "ImprovementJobLeased", "", job.RootTaskID, map[string]any{"owner": owner, "until_ms": job.LeaseUntilMS}); err != nil {
		return nil, err
	}
	return &job, tx.Commit()
}

func (s *Store) ReleaseImprovementJob(ctx context.Context, jobID, owner string, cause error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := readJSONRow[model.ImprovementJob](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_job WHERE job_id=?`, jobID))
	if err != nil {
		return err
	}
	if job.State != "LEASED" || job.LeaseOwner != owner {
		return fmt.Errorf("%w: improvement lease changed", model.ErrConflict)
	}
	job.State, job.LeaseOwner, job.LeaseUntilMS, job.Attempts, job.UpdatedAtMS = "PENDING", "", 0, job.Attempts+1, time.Now().UnixMilli()
	job.AvailableAtMS = time.Now().Add(time.Duration(minInt(job.Attempts, 30)) * time.Second).UnixMilli()
	if cause != nil {
		job.Error = redactObservation(cause.Error())
	}
	raw, _ := json.Marshal(job)
	if _, err = tx.ExecContext(ctx, `UPDATE improvement_job SET state=?,available_at_ms=?,lease_owner='',lease_until_ms=0,attempts=?,data_json=? WHERE job_id=?`, job.State, job.AvailableAtMS, job.Attempts, raw, job.ID); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "improvement_job", job.ID, "ImprovementJobDeferred", "", job.RootTaskID, map[string]any{"attempts": job.Attempts, "error": job.Error})
	if err != nil {
		return err
	}
	return tx.Commit()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Store) StartImprovementJob(ctx context.Context, jobID, owner string, req CreateRunRequest, instructions string, schema json.RawMessage) (model.Run, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, err
	}
	defer tx.Rollback()
	job, err := readJSONRow[model.ImprovementJob](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_job WHERE job_id=?`, jobID))
	if err != nil {
		return model.Run{}, err
	}
	if job.State != "LEASED" || job.LeaseOwner != owner || job.LeaseUntilMS < time.Now().UnixMilli() {
		return model.Run{}, fmt.Errorf("%w: improvement lease expired", model.ErrConflict)
	}
	title := "持续改进分析"
	if job.Kind == "JUDGE" {
		title = "持续改进盲评"
	}
	internal, _, err := createTaskTx(ctx, tx, "", title, "Analyze recorded evidence only. Do not perform or modify the business task.")
	if err != nil {
		return model.Run{}, err
	}
	job.InternalTaskID = internal.ID
	job.State, job.UpdatedAtMS = "STARTING", time.Now().UnixMilli()
	raw, _ := json.Marshal(job)
	if _, err = tx.ExecContext(ctx, `UPDATE improvement_job SET state=?,internal_task_id=?,data_json=? WHERE job_id=?`, job.State, job.InternalTaskID, raw, job.ID); err != nil {
		return model.Run{}, err
	}
	req.TaskID, req.ImprovementJobID, req.ReadOnly, req.OutputSchema, req.Instructions = internal.ID, job.ID, true, schema, instructions
	req.IdempotencyKey = fmt.Sprintf("improvement:%s:%d", job.ID, job.Attempts)
	run, err := createRunTx(ctx, tx, req)
	if err != nil {
		return run, err
	}
	job.RunID, job.State, job.LeaseOwner, job.LeaseUntilMS, job.UpdatedAtMS = run.ID, "RUNNING", "", 0, time.Now().UnixMilli()
	raw, _ = json.Marshal(job)
	if _, err = tx.ExecContext(ctx, `UPDATE improvement_job SET state='RUNNING',lease_owner='',lease_until_ms=0,run_id=?,data_json=? WHERE job_id=?`, run.ID, raw, job.ID); err != nil {
		return run, err
	}
	if _, err = appendEventTx(ctx, tx, "improvement_job", job.ID, "ImprovementJobStarted", run.ID, job.RootTaskID, map[string]any{"kind": job.Kind, "run_id": run.ID}); err != nil {
		return run, err
	}
	return run, tx.Commit()
}

func applyImprovementResultTx(ctx context.Context, tx *sql.Tx, event model.RuntimeEvent) error {
	job, err := readJSONRow[model.ImprovementJob](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_job WHERE run_id=?`, event.RunID))
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if job.State != "RUNNING" || job.RunID != event.RunID {
		return nil
	}
	now := time.Now().UnixMilli()
	if event.Type != "run.completed" {
		job.State, job.RunID, job.InternalTaskID, job.Attempts, job.UpdatedAtMS = "PENDING", "", "", job.Attempts+1, now
		job.AvailableAtMS = now + int64(minInt(job.Attempts, 30))*1000
		job.Error = redactObservation(event.Error)
		return saveImprovementJobTx(ctx, tx, job, "ImprovementJobRetryScheduled")
	}
	switch job.Kind {
	case "ANALYZE":
		var result struct {
			Proposals []model.CandidateProposal `json:"proposals"`
		}
		if err = strictDecode([]byte(event.Output), &result); err != nil || len(result.Proposals) > 10 {
			if err == nil {
				err = errors.New("analyst returned more than 10 proposals")
			}
			job.State, job.Error, job.UpdatedAtMS = "FAILED", redactObservation(err.Error()), now
			return saveImprovementJobTx(ctx, tx, job, "ImprovementJobFailed")
		}
		for _, proposal := range result.Proposals {
			if _, err = createCandidateTx(ctx, tx, job, proposal); err != nil {
				return err
			}
		}
	case "JUDGE":
		var judgement model.SemanticJudgement
		if err = strictDecode([]byte(event.Output), &judgement); err != nil || judgement.Score < 0 || judgement.Score > 100 || strings.TrimSpace(judgement.Reason) == "" || len(judgement.Reason) > 4000 {
			if err == nil {
				err = errors.New("judge score/reason is invalid")
			}
			job.State, job.Error, job.UpdatedAtMS = "FAILED", redactObservation(err.Error()), now
			return saveImprovementJobTx(ctx, tx, job, "ImprovementJobFailed")
		}
		if err = applyJudgementTx(ctx, tx, job.RootTaskID, judgement); err != nil {
			return err
		}
	default:
		return errors.New("unknown improvement job kind")
	}
	job.State, job.Error, job.UpdatedAtMS = "COMPLETED", "", now
	return saveImprovementJobTx(ctx, tx, job, "ImprovementJobCompleted")
}

func strictDecode(raw []byte, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("extra JSON content")
	}
	return nil
}

func saveImprovementJobTx(ctx context.Context, tx *sql.Tx, job model.ImprovementJob, eventType string) error {
	raw, _ := json.Marshal(job)
	var internalTask, runID any
	if job.InternalTaskID != "" {
		internalTask = job.InternalTaskID
	}
	if job.RunID != "" {
		runID = job.RunID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE improvement_job SET state=?,available_at_ms=?,lease_owner=?,lease_until_ms=?,attempts=?,internal_task_id=?,run_id=?,data_json=? WHERE job_id=?`, job.State, job.AvailableAtMS, job.LeaseOwner, job.LeaseUntilMS, job.Attempts, internalTask, runID, raw, job.ID); err != nil {
		return err
	}
	_, err := appendEventTx(ctx, tx, "improvement_job", job.ID, eventType, job.RunID, job.RootTaskID, map[string]any{"state": job.State, "attempts": job.Attempts, "error": job.Error})
	return err
}

func createCandidateTx(ctx context.Context, tx *sql.Tx, job model.ImprovementJob, proposal model.CandidateProposal) (model.ImprovementCandidate, error) {
	now := time.Now().UnixMilli()
	rawFingerprint, _ := json.Marshal(struct {
		Type  string
		Scope model.ImprovementScope
		Patch json.RawMessage
	}{proposal.Type, proposal.Scope, proposal.Patch})
	digest := sha256.Sum256(rawFingerprint)
	fingerprint := hex.EncodeToString(digest[:])
	existing, err := readJSONRow[model.ImprovementCandidate](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_candidate WHERE fingerprint=?`, fingerprint))
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return existing, err
	}
	candidate := model.ImprovementCandidate{ID: id.New("candidate"), Type: proposal.Type, Title: proposal.Title, Rationale: proposal.Rationale, Scope: proposal.Scope, Patch: proposal.Patch, Evidence: proposal.Evidence, SourceTaskID: job.RootTaskID, SourceRunID: job.RunID, State: model.CandidateStateDiscovered, Version: 1, CreatedAtMS: now, UpdatedAtMS: now}
	data, _ := json.Marshal(candidate)
	if _, err = tx.ExecContext(ctx, `INSERT INTO improvement_candidate VALUES(?,?,?,?,?,?,?,?,?)`, candidate.ID, candidate.Type, candidate.State, candidate.Version, candidate.SourceTaskID, now, now, fingerprint, data); err != nil {
		return candidate, err
	}
	if _, err = appendEventTx(ctx, tx, "improvement_candidate", candidate.ID, "ImprovementCandidateDiscovered", job.RunID, job.RootTaskID, candidate); err != nil {
		return candidate, err
	}
	profile, profileErr := readJSONRow[model.TaskProfile](tx.QueryRowContext(ctx, `SELECT data_json FROM task_profile WHERE root_task_id=?`, job.RootTaskID))
	if profileErr != nil {
		return candidate, profileErr
	}
	validateErr := improvement.ValidateProposal(proposal, profile)
	if validateErr == nil {
		if job.Observation == nil {
			validateErr = errors.New("candidate has no source observation")
		} else {
			validateErr = improvement.ValidateProposalEvidence(proposal, *job.Observation)
		}
	}
	if validateErr == nil {
		validateErr = candidateFieldLockErrorTx(ctx, tx, proposal.Type, proposal.Scope, proposal.Patch, candidate.ID)
	}
	if validateErr == nil && proposal.Type == "experience" {
		var patch model.ExperiencePatch
		if decodeErr := json.Unmarshal(proposal.Patch, &patch); decodeErr != nil {
			validateErr = decodeErr
		} else if patch.Operation == "revise" || patch.Operation == "retire" {
			current, readErr := readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience WHERE experience_id=?`, patch.ExperienceID))
			if readErr != nil {
				validateErr = errors.New("target experience does not exist")
			} else if current.Revision != patch.ExpectedRevision || current.State != "ACTIVE" || current.Scope != proposal.Scope {
				validateErr = errors.New("target experience revision, state, or scope changed")
			}
		}
	}
	if validateErr != nil {
		candidate.State, candidate.ValidationError = model.CandidateStateRejected, validateErr.Error()
	} else {
		candidate.State = model.CandidateStateValidated
	}
	candidate.Version++
	if err = saveCandidateTx(ctx, tx, candidate, "ImprovementCandidate"+candidate.State); err != nil {
		return candidate, err
	}
	if candidate.State == model.CandidateStateValidated {
		readiness, readyErr := improvementReadinessTx(ctx, tx)
		if readyErr != nil {
			return candidate, readyErr
		}
		if readiness.Ready {
			candidate, err = activateCandidateTx(ctx, tx, candidate)
			if errors.Is(err, model.ErrConflict) {
				// A valid candidate remains queued while another overlapping
				// experiment is active; this is ordinary serialization, not an
				// Analyst/job failure.
				err = nil
			}
		}
	}
	return candidate, err
}

func improvementReadinessTx(ctx context.Context, tx *sql.Tx) (model.ImprovementReadiness, error) {
	var result model.ImprovementReadiness
	if err := tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM improvement_meta WHERE key='observation_started_at_ms'`).Scan(&result.ObservationStartedAtMS); err != nil {
		return result, err
	}
	result.ObservationHours = float64(time.Now().UnixMilli()-result.ObservationStartedAtMS) / float64(time.Hour.Milliseconds())
	var total, covered int
	if err := tx.QueryRowContext(ctx, `WITH RECURSIVE scoped(root_task_id,task_id) AS (
		SELECT root_task_id,root_task_id FROM experiment_assignment
		UNION SELECT s.root_task_id,e.to_task_id FROM scoped s JOIN task_edge e ON e.from_task_id=s.task_id WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS')
	) SELECT COUNT(*),COALESCE(SUM(CASE WHEN NOT EXISTS(
		SELECT 1 FROM scoped s JOIN run r ON r.task_id=s.task_id
		WHERE s.root_task_id=a.root_task_id AND r.state NOT IN ('QUEUED','RUNNING')
		AND NOT EXISTS(SELECT 1 FROM run_token_usage u WHERE u.run_id=r.run_id)
	) THEN 1 ELSE 0 END),0)
	FROM experiment_assignment a
	WHERE EXISTS(SELECT 1 FROM task_summary summary WHERE summary.task_id=a.root_task_id AND summary.completed_at_ms>=?)`, result.ObservationStartedAtMS).Scan(&total, &covered); err != nil {
		return result, err
	}
	if total > 0 {
		result.DataCoverage = float64(covered) * 100 / float64(total)
	}
	result.Ready = result.ObservationHours >= 24 && result.DataCoverage >= 80
	switch {
	case result.ObservationHours < 24:
		result.Reason = "需要先完成 24 小时观测完整性检查"
	case result.DataCoverage < 80:
		result.Reason = fmt.Sprintf("关键数据覆盖率 %.1f%%，低于 80%%", result.DataCoverage)
	default:
		result.Reason = "观测时间和关键数据覆盖率已达到首个实验门槛"
	}
	return result, nil
}

func (s *Store) ImprovementReadiness(ctx context.Context) (model.ImprovementReadiness, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.ImprovementReadiness{}, err
	}
	defer tx.Rollback()
	return improvementReadinessTx(ctx, tx)
}

func (s *Store) ActivateValidatedCandidates(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	readiness, err := improvementReadinessTx(ctx, tx)
	if err != nil || !readiness.Ready {
		return err
	}
	items, err := listJSONRows[model.ImprovementCandidate](ctx, tx, `SELECT data_json FROM improvement_candidate WHERE state='VALIDATED' ORDER BY created_at_ms LIMIT 10`)
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, err = activateCandidateTx(ctx, tx, item); err != nil && !errors.Is(err, model.ErrConflict) {
			return err
		}
	}
	return tx.Commit()
}

func activateCandidateTx(ctx context.Context, tx *sql.Tx, candidate model.ImprovementCandidate) (model.ImprovementCandidate, error) {
	if candidate.State != model.CandidateStateValidated {
		return candidate, fmt.Errorf("%w: candidate is not validated", model.ErrConflict)
	}
	if lockErr := candidateFieldLockErrorTx(ctx, tx, candidate.Type, candidate.Scope, candidate.Patch, candidate.ID); lockErr != nil {
		candidate.State, candidate.ValidationError = model.CandidateStateRejected, lockErr.Error()
		candidate.Version++
		candidate.UpdatedAtMS = time.Now().UnixMilli()
		return candidate, saveCandidateTx(ctx, tx, candidate, "ImprovementCandidateRejectedByFieldLock")
	}
	active, err := listJSONRows[model.ImprovementExperiment](ctx, tx, `SELECT data_json FROM improvement_experiment WHERE state IN ('CANARY','PROMOTED')`)
	if err != nil {
		return candidate, err
	}
	for _, experiment := range active {
		if scopesOverlap(experiment.Scope, candidate.Scope) {
			return candidate, fmt.Errorf("%w: one matching experiment is already active", model.ErrConflict)
		}
	}
	now := time.Now().UnixMilli()
	experiment := model.ImprovementExperiment{ID: id.New("experiment"), CandidateID: candidate.ID, Scope: candidate.Scope, State: model.CandidateStateCanary, BaselinePolicyID: defaultPolicyID, BaselinePolicyVersion: 1, CreatedAtMS: now, UpdatedAtMS: now}
	baseline, err := activePolicyForScopeTx(ctx, tx, candidate.Scope)
	if err != nil {
		return candidate, err
	}
	experiment.BaselinePolicyID, experiment.BaselinePolicyVersion = baseline.ID, baseline.Version
	if candidate.Type == "experience" {
		var patch model.ExperiencePatch
		if err = json.Unmarshal(candidate.Patch, &patch); err != nil {
			return candidate, err
		}
		if patch.Operation == "" {
			patch.Operation = "add"
		}
		experiment.ExperienceOperation = patch.Operation
		switch patch.Operation {
		case "add":
			experience, createErr := experienceFromCandidateTx(ctx, tx, candidate, model.CandidateStateCanary)
			if createErr != nil {
				return candidate, createErr
			}
			experiment.CandidateExperienceID = experience.ID
			experiment.CandidateExperienceRev = experience.Revision
		case "revise", "retire":
			current, readErr := readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience WHERE experience_id=?`, patch.ExperienceID))
			if readErr != nil {
				return candidate, readErr
			}
			if current.Revision != patch.ExpectedRevision || current.State != "ACTIVE" || current.Scope != candidate.Scope {
				return candidate, fmt.Errorf("%w: target experience revision, state, or scope changed", model.ErrConflict)
			}
			experiment.CandidateExperienceID = current.ID
			experiment.BaselineExperienceRev = current.Revision
			if patch.Operation == "revise" {
				staged, stageErr := stageExperienceRevisionTx(ctx, tx, current, patch, candidate)
				if stageErr != nil {
					return candidate, stageErr
				}
				experiment.CandidateExperienceRev = staged.Revision
			}
		default:
			return candidate, fmt.Errorf("%w: invalid experience operation", model.ErrValidation)
		}
	} else {
		policy, createErr := policyFromCandidateTx(ctx, tx, baseline, candidate)
		if createErr != nil {
			return candidate, createErr
		}
		experiment.CandidatePolicyID, experiment.CandidatePolicyVersion = policy.ID, policy.Version
	}
	data, _ := json.Marshal(experiment)
	if _, err = tx.ExecContext(ctx, `INSERT INTO improvement_experiment VALUES(?,?,?,?,?,?)`, experiment.ID, candidate.ID, experiment.State, now, now, data); err != nil {
		return candidate, err
	}
	candidate.State, candidate.ExperimentID, candidate.Version, candidate.UpdatedAtMS = model.CandidateStateCanary, experiment.ID, candidate.Version+1, now
	if err = saveCandidateTx(ctx, tx, candidate, "ImprovementCandidateCanary"); err != nil {
		return candidate, err
	}
	_, err = appendEventTx(ctx, tx, "improvement_experiment", experiment.ID, "ImprovementExperimentStarted", candidate.ID, candidate.SourceTaskID, experiment)
	return candidate, err
}

func activePolicyForScopeTx(ctx context.Context, tx *sql.Tx, scope model.ImprovementScope) (model.OptimizationPolicy, error) {
	items, err := listJSONRows[model.OptimizationPolicy](ctx, tx, `SELECT data_json FROM optimization_policy WHERE state='ACTIVE' ORDER BY created_at_ms DESC`)
	if err != nil {
		return model.OptimizationPolicy{}, err
	}
	for _, item := range items {
		if scopesOverlap(scope, item.Scope) {
			return item, nil
		}
	}
	return getPolicyTx(ctx, tx, defaultPolicyID, 1)
}

func scopesOverlap(a, b model.ImprovementScope) bool {
	checks := [][2]string{{a.SourceType, b.SourceType}, {a.TaskType, b.TaskType}, {a.Repository, b.Repository}, {a.RoleID, b.RoleID}, {a.Workflow, b.Workflow}, {a.RuntimeOS, b.RuntimeOS}}
	for _, check := range checks {
		if check[0] != "" && check[1] != "" && check[0] != check[1] {
			return false
		}
	}
	return true
}

func policyPatchFields(raw json.RawMessage) ([]string, error) {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || patch == nil {
		return nil, errors.New("policy patch must be a JSON object")
	}
	fields := make([]string, 0, len(patch))
	for field := range patch {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields, nil
}

func policyLockFields(candidateType string, requested []string) ([]string, error) {
	allowed := map[string]map[string]bool{
		"prompt":   {"role_id": true, "stage": true, "overlay": true},
		"routing":  {"load_weight": true, "success_weight": true, "cost_weight": true},
		"recovery": {"continuous_run": true, "backoff_ms": true, "split_environment_after": true},
		"test":     {"risk_mode": true, "reviewer_count": true, "retest_mode": true},
		"workflow": {"require_agent_plan_review": true, "require_human_plan_review": true, "require_pr_review": true, "require_passing_tests": true, "require_human_acceptance": true, "plan_review_round_limit": true},
	}[candidateType]
	if allowed == nil {
		return nil, fmt.Errorf("%w: policy field locks require a predefined policy candidate", model.ErrValidation)
	}
	seen := map[string]bool{}
	fields := make([]string, 0, len(requested))
	for _, field := range requested {
		field = strings.TrimSpace(field)
		if !allowed[field] {
			return nil, fmt.Errorf("%w: %s.%s is not a lockable predefined field", model.ErrValidation, candidateType, field)
		}
		if !seen[field] {
			seen[field] = true
			fields = append(fields, field)
		}
	}
	sort.Strings(fields)
	return fields, nil
}

func changedPolicyFieldsTx(ctx context.Context, tx *sql.Tx, candidate model.ImprovementCandidate) ([]string, error) {
	if candidate.Type == "experience" {
		return nil, fmt.Errorf("%w: experience content is versioned, not field-locked", model.ErrValidation)
	}
	baseline, err := activePolicyForScopeTx(ctx, tx, candidate.Scope)
	if candidate.ExperimentID != "" {
		var experiment model.ImprovementExperiment
		experiment, err = readJSONRow[model.ImprovementExperiment](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_experiment WHERE experiment_id=?`, candidate.ExperimentID))
		if err == nil {
			baseline, err = getPolicyTx(ctx, tx, experiment.BaselinePolicyID, experiment.BaselinePolicyVersion)
		}
	}
	if err != nil {
		return nil, err
	}
	var component any
	switch candidate.Type {
	case "prompt":
		component = baseline.Prompt
	case "routing":
		component = baseline.Routing
	case "recovery":
		component = baseline.Recovery
	case "test":
		component = baseline.Test
	case "workflow":
		component = baseline.Workflow
	default:
		return nil, fmt.Errorf("%w: unsupported policy candidate type", model.ErrValidation)
	}
	baseRaw, _ := json.Marshal(component)
	var base, patch map[string]any
	if err = json.Unmarshal(baseRaw, &base); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(candidate.Patch, &patch); err != nil || patch == nil {
		return nil, errors.New("policy patch must be a JSON object")
	}
	fields := make([]string, 0, len(patch))
	for field, value := range patch {
		if !reflect.DeepEqual(base[field], value) {
			fields = append(fields, field)
		}
	}
	sort.Strings(fields)
	if len(fields) == 0 {
		return nil, fmt.Errorf("%w: candidate has no policy field change to lock", model.ErrValidation)
	}
	return fields, nil
}

func candidateFieldLockErrorTx(ctx context.Context, tx *sql.Tx, candidateType string, scope model.ImprovementScope, patch json.RawMessage, excludeID string) error {
	if candidateType == "experience" {
		return nil
	}
	fields, err := policyPatchFields(patch)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(fields))
	for _, field := range fields {
		wanted[field] = true
	}
	locked, err := listJSONRows[model.ImprovementCandidate](ctx, tx, `SELECT data_json FROM improvement_candidate ORDER BY created_at_ms`)
	if err != nil {
		return err
	}
	for _, item := range locked {
		if item.ID == excludeID || item.Type != candidateType || len(item.LockedFields) == 0 || !scopesOverlap(item.Scope, scope) {
			continue
		}
		for _, field := range item.LockedFields {
			if wanted[field] {
				return fmt.Errorf("%w: %s.%s by candidate %s", errPolicyFieldLocked, candidateType, field, item.ID)
			}
		}
	}
	return nil
}

func rejectCandidatesCoveredByLockTx(ctx context.Context, tx *sql.Tx, lock model.ImprovementCandidate) error {
	items, err := listJSONRows[model.ImprovementCandidate](ctx, tx, `SELECT data_json FROM improvement_candidate WHERE state='VALIDATED' AND candidate_id<>? ORDER BY created_at_ms`, lock.ID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err = candidateFieldLockErrorTx(ctx, tx, item.Type, item.Scope, item.Patch, item.ID); err == nil {
			continue
		} else if !errors.Is(err, errPolicyFieldLocked) {
			return err
		}
		item.State, item.ValidationError = model.CandidateStateRejected, err.Error()
		item.Version++
		item.UpdatedAtMS = time.Now().UnixMilli()
		if err = saveCandidateTx(ctx, tx, item, "ImprovementCandidateRejectedByFieldLock"); err != nil {
			return err
		}
	}
	return nil
}

func nextExperienceGenerationTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var generation int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(json_extract(data_json,'$.generation') AS INTEGER)),0)+1 FROM experience_revision`).Scan(&generation)
	return generation, err
}

func nextExperienceRevisionTx(ctx context.Context, tx *sql.Tx, experienceID string) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM experience_revision WHERE experience_id=?`, experienceID).Scan(&revision)
	return revision, err
}

func insertExperienceRevisionTx(ctx context.Context, tx *sql.Tx, item model.Experience) error {
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO experience_revision VALUES(?,?,?,?)`, item.ID, item.Revision, raw, item.UpdatedAtMS)
	return err
}

func stageExperienceRevisionTx(ctx context.Context, tx *sql.Tx, current model.Experience, patch model.ExperiencePatch, candidate model.ImprovementCandidate) (model.Experience, error) {
	revision, err := nextExperienceRevisionTx(ctx, tx, current.ID)
	if err != nil {
		return model.Experience{}, err
	}
	generation, err := nextExperienceGenerationTx(ctx, tx)
	if err != nil {
		return model.Experience{}, err
	}
	now := time.Now().UnixMilli()
	staged := current
	staged.Revision, staged.Version, staged.Generation = revision, current.Version+1, generation
	staged.State, staged.Situation, staged.Action = model.CandidateStateCanary, patch.Situation, patch.Action
	staged.Avoid, staged.Verification, staged.Evidence = patch.Avoid, patch.Verification, candidate.Evidence
	staged.SampleCount, staged.EffectScore, staged.UpdatedAtMS = 0, 0, now
	if err = insertExperienceRevisionTx(ctx, tx, staged); err != nil {
		return staged, err
	}
	_, err = appendEventTx(ctx, tx, "experience", staged.ID, "ExperienceRevisionStaged", candidate.ID, candidate.SourceTaskID, map[string]any{"revision": staged.Revision, "generation": staged.Generation, "candidate_id": candidate.ID})
	return staged, err
}

// publishExperienceRevisionTx advances the catalog head by appending an
// immutable snapshot. Previously pinned task generations continue to read the
// exact older revision from experience_revision.
func publishExperienceRevisionTx(ctx context.Context, tx *sql.Tx, current, content model.Experience, state string, candidate model.ImprovementCandidate, experiment model.ImprovementExperiment, eventType string) (model.Experience, error) {
	revision, err := nextExperienceRevisionTx(ctx, tx, current.ID)
	if err != nil {
		return current, err
	}
	generation, err := nextExperienceGenerationTx(ctx, tx)
	if err != nil {
		return current, err
	}
	now := time.Now().UnixMilli()
	published := content
	published.ID, published.CreatedAtMS = current.ID, current.CreatedAtMS
	published.Revision, published.Version, published.Generation = revision, current.Version+1, generation
	published.State, published.UpdatedAtMS = state, now
	if eventType != "ExperienceRevisionRolledBack" {
		published.SampleCount = experiment.CandidateSamples + experiment.PromotedSamples
		published.EffectScore = experiment.Score.Composite
	}
	if err = insertExperienceRevisionTx(ctx, tx, published); err != nil {
		return current, err
	}
	raw, _ := json.Marshal(published)
	if _, err = tx.ExecContext(ctx, `UPDATE experience SET current_revision=?,state=?,generation=?,version=?,updated_at_ms=?,data_json=? WHERE experience_id=?`, published.Revision, published.State, published.Generation, published.Version, now, raw, published.ID); err != nil {
		return current, err
	}
	_, err = appendEventTx(ctx, tx, "experience", published.ID, eventType, candidate.ID, candidate.SourceTaskID, map[string]any{"state": state, "revision": published.Revision, "generation": published.Generation, "reason": experiment.DecisionReason, "samples": published.SampleCount, "effect_score": published.EffectScore})
	return published, err
}

func experienceFromCandidateTx(ctx context.Context, tx *sql.Tx, candidate model.ImprovementCandidate, state string) (model.Experience, error) {
	var patch model.ExperiencePatch
	if err := json.Unmarshal(candidate.Patch, &patch); err != nil {
		return model.Experience{}, err
	}
	now := time.Now().UnixMilli()
	generation, err := nextExperienceGenerationTx(ctx, tx)
	if err != nil {
		return model.Experience{}, err
	}
	item := model.Experience{ID: id.New("experience"), Revision: 1, Version: 1, Generation: generation, State: state, Situation: patch.Situation, Action: patch.Action, Avoid: patch.Avoid, Verification: patch.Verification, Scope: candidate.Scope, Evidence: candidate.Evidence, CreatedAtMS: now, UpdatedAtMS: now}
	raw, _ := json.Marshal(item)
	if _, err := tx.ExecContext(ctx, `INSERT INTO experience VALUES(?,?,?,?,?,?,?)`, item.ID, item.Revision, item.State, item.Generation, item.Version, now, raw); err != nil {
		return item, err
	}
	if err := insertExperienceRevisionTx(ctx, tx, item); err != nil {
		return item, err
	}
	_, err = appendEventTx(ctx, tx, "experience", item.ID, "ExperienceCreated", candidate.ID, candidate.SourceTaskID, item)
	return item, err
}

func policyFromCandidateTx(ctx context.Context, tx *sql.Tx, baseline model.OptimizationPolicy, candidate model.ImprovementCandidate) (model.OptimizationPolicy, error) {
	policy := baseline
	policy.ID = id.New("policy")
	policy.Version = 1
	policy.State = "CANARY"
	policy.Scope = candidate.Scope
	policy.CreatedAtMS = time.Now().UnixMilli()
	switch candidate.Type {
	case "prompt":
		if err := json.Unmarshal(candidate.Patch, &policy.Prompt); err != nil {
			return policy, err
		}
	case "routing":
		if err := json.Unmarshal(candidate.Patch, &policy.Routing); err != nil {
			return policy, err
		}
	case "recovery":
		if err := json.Unmarshal(candidate.Patch, &policy.Recovery); err != nil {
			return policy, err
		}
	case "test":
		if err := json.Unmarshal(candidate.Patch, &policy.Test); err != nil {
			return policy, err
		}
	case "workflow":
		if err := json.Unmarshal(candidate.Patch, &policy.Workflow); err != nil {
			return policy, err
		}
	default:
		return policy, errors.New("not a policy candidate")
	}
	raw, _ := json.Marshal(policy)
	_, err := tx.ExecContext(ctx, `INSERT INTO optimization_policy VALUES(?,?,?,?,?)`, policy.ID, policy.Version, policy.State, policy.CreatedAtMS, raw)
	if err == nil {
		_, err = appendEventTx(ctx, tx, "optimization_policy", policy.ID, "OptimizationPolicyCreated", candidate.ID, candidate.SourceTaskID, policy)
	}
	return policy, err
}

func saveCandidateTx(ctx context.Context, tx *sql.Tx, candidate model.ImprovementCandidate, eventType string) error {
	raw, _ := json.Marshal(candidate)
	if _, err := tx.ExecContext(ctx, `UPDATE improvement_candidate SET state=?,version=?,updated_at_ms=?,data_json=? WHERE candidate_id=?`, candidate.State, candidate.Version, candidate.UpdatedAtMS, raw, candidate.ID); err != nil {
		return err
	}
	_, err := appendEventTx(ctx, tx, "improvement_candidate", candidate.ID, eventType, "", candidate.SourceTaskID, candidate)
	return err
}

func prepareEvaluationTx(ctx context.Context, tx *sql.Tx, rootTaskID string) (model.TaskEvaluation, model.EvaluationInput, error) {
	assignment, err := readJSONRow[model.OptimizationAssignment](tx.QueryRowContext(ctx, `SELECT data_json FROM experiment_assignment WHERE root_task_id=?`, rootTaskID))
	if err != nil {
		return model.TaskEvaluation{}, model.EvaluationInput{}, err
	}
	summary, err := readJSONRow[model.TaskSummary](tx.QueryRowContext(ctx, `SELECT data_json FROM task_summary WHERE task_id=? ORDER BY version DESC LIMIT 1`, rootTaskID))
	if err != nil {
		return model.TaskEvaluation{}, model.EvaluationInput{}, err
	}
	taskType := assignment.Profile.TaskType
	codeTask := taskType == "code" || taskType == "bug"
	evaluation := model.TaskEvaluation{ID: id.New("evaluation"), TaskID: rootTaskID, RootTaskID: rootTaskID, ExperimentID: assignment.ExperimentID, Arm: assignment.Arm, TaskType: taskType, Evidence: []model.EvaluationEvidence{}, CycleTimeMS: summary.Metrics.CycleTimeMS, CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
	if existing, readErr := readJSONRow[model.TaskEvaluation](tx.QueryRowContext(ctx, `SELECT data_json FROM task_evaluation WHERE task_id=?`, rootTaskID)); readErr == nil {
		evaluation.ID, evaluation.CreatedAtMS, evaluation.JudgeScore, evaluation.JudgeReason = existing.ID, existing.CreatedAtMS, existing.JudgeScore, existing.JudgeReason
		evaluation.FailedSample, evaluation.SafetyViolation = existing.FailedSample, existing.SafetyViolation
	} else if readErr != sql.ErrNoRows {
		return evaluation, model.EvaluationInput{}, readErr
	}
	var due int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(available_at_ms),0) FROM improvement_job WHERE root_task_id=? AND kind='JUDGE'`, rootTaskID).Scan(&due); err != nil {
		return evaluation, model.EvaluationInput{}, err
	}
	evaluation.ObservationDueAtMS = due
	const taskTree = `WITH RECURSIVE scope(task_id) AS (
		SELECT ? UNION SELECT e.to_task_id FROM task_edge e JOIN scope s ON e.from_task_id=s.task_id
		WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS')
	)`
	// Score the root delivery as one unit. Initial task messages are inputs, not
	// human corrections, so subtract one per business task just as TaskSummary
	// does. Improvement tasks are not connected to this graph and stay excluded.
	if err = tx.QueryRowContext(ctx, taskTree+` SELECT
		COALESCE((SELECT SUM(CASE WHEN message_count>0 THEN message_count-1 ELSE 0 END) FROM (
			SELECT m.task_id,COUNT(*) message_count FROM task_message m JOIN scope s ON s.task_id=m.task_id WHERE m.speaker='user' GROUP BY m.task_id
		)),0)
		+ (SELECT COUNT(*) FROM review r JOIN scope s ON s.task_id=r.task_id WHERE r.state='CHANGES_REQUESTED')
		+ (SELECT COUNT(*) FROM event_log event JOIN scope s ON s.task_id=event.aggregate_id WHERE event.aggregate_type='task' AND event.event_type='ExecutionPermissionDecided'),
		(SELECT COUNT(*) FROM run r JOIN scope s ON s.task_id=r.task_id WHERE r.state='FAILED' OR (r.state='COMPLETED' AND length(trim(COALESCE(r.output,'')))=0))`, rootTaskID).Scan(&evaluation.HumanInterventions, &evaluation.NoProgressRuns); err != nil {
		return evaluation, model.EvaluationInput{}, err
	}
	if codeTask {
		var reviewTotal, reviewDone int
		if err = tx.QueryRowContext(ctx, taskTree+` SELECT COUNT(*),COALESCE(SUM(r.state='COMPLETED' AND COALESCE((
			SELECT CASE WHEN json_valid(latest.output) THEN json_extract(latest.output,'$.review_decision') ELSE '' END
			FROM run latest WHERE latest.task_id=r.task_id AND latest.state='COMPLETED'
			ORDER BY latest.created_at_ms DESC LIMIT 1
		),'')='passed'),0)
			FROM source_review r JOIN source_target t ON t.target_id=r.target_id JOIN scope s ON s.task_id=t.task_id
			WHERE r.state!='SUPERSEDED' AND r.head_sha=json_extract(t.data_json,'$.head_sha')`, rootTaskID).Scan(&reviewTotal, &reviewDone); err != nil {
			return evaluation, model.EvaluationInput{}, err
		}
		evaluation.Evidence = append(evaluation.Evidence, qualityEvidence("current_commit_reviewer", 25, reviewTotal > 0, reviewTotal > 0 && reviewDone == reviewTotal, fmt.Sprintf("%d/%d reviewers completed", reviewDone, reviewTotal)))
		var pipelines, passed int
		if err = tx.QueryRowContext(ctx, taskTree+` SELECT COUNT(*),COALESCE(SUM(p.state IN ('success','PASSED','SUCCESS','COMPLETED')),0)
			FROM test_pipeline p JOIN source_target t ON t.target_id=p.pr_target_id JOIN scope s ON s.task_id=t.task_id
			WHERE p.head_sha=json_extract(t.data_json,'$.head_sha')
			AND p.attempt=(SELECT MAX(latest.attempt) FROM test_pipeline latest WHERE latest.pr_target_id=p.pr_target_id AND latest.head_sha=p.head_sha)`, rootTaskID).Scan(&pipelines, &passed); err != nil {
			return evaluation, model.EvaluationInput{}, err
		}
		evaluation.Evidence = append(evaluation.Evidence, qualityEvidence("ci_pipeline", 25, pipelines > 0, pipelines > 0 && passed == pipelines, fmt.Sprintf("%d/%d pipelines passed", passed, pipelines)))
		var targets, merged int
		if err = tx.QueryRowContext(ctx, taskTree+` SELECT COUNT(*),COALESCE(SUM(EXISTS(
			SELECT 1 FROM source_event e WHERE e.target_id=t.target_id AND e.state='RECORDED'
			AND json_extract(e.data_json,'$.kind')='github.merged' AND json_extract(e.data_json,'$.head_sha')=json_extract(t.data_json,'$.head_sha'))),0)
			FROM source_target t JOIN task_source source ON source.source_id=t.source_id JOIN scope s ON s.task_id=t.task_id
			WHERE json_extract(source.data_json,'$.kind')='github'`, rootTaskID).Scan(&targets, &merged); err != nil {
			return evaluation, model.EvaluationInput{}, err
		}
		evaluation.Evidence = append(evaluation.Evidence, qualityEvidence("pr_merged", 20, targets > 0, targets > 0 && merged == targets, fmt.Sprintf("%d/%d PR targets merged", merged, targets)))
		// A terminal source record is independent from the PR merge record. Only
		// the latest lifecycle fact counts: a later reopen makes the evidence
		// available-but-failing instead of preserving an obsolete passing score.
		var sourceLifecycle string
		lifecycleErr := tx.QueryRowContext(ctx, `SELECT event_type FROM event_log
			WHERE aggregate_type='task' AND aggregate_id=?
			AND event_type IN ('ExternalTaskClosed','SourceTaskCompleted','ExternalTaskReopened','SourceTaskReopened')
			ORDER BY global_seq DESC LIMIT 1`, rootTaskID).Scan(&sourceLifecycle)
		if lifecycleErr != nil && lifecycleErr != sql.ErrNoRows {
			return evaluation, model.EvaluationInput{}, lifecycleErr
		}
		terminal := sourceLifecycle == "ExternalTaskClosed" || sourceLifecycle == "SourceTaskCompleted"
		evaluation.Evidence = append(evaluation.Evidence, qualityEvidence("source_terminal_not_reopened", 15, lifecycleErr == nil, terminal, sourceLifecycle))
		evaluation.Evidence = append(evaluation.Evidence, qualityEvidence("human_acceptance", 10, summary.SourceType == "review", summary.SourceType == "review", summary.AcceptanceComment))
	} else {
		evaluation.Evidence = append(evaluation.Evidence, qualityEvidence("human_acceptance", 60, summary.SourceType == "review", summary.SourceType == "review", summary.AcceptanceComment))
	}
	judgeWeight := 40.0
	if codeTask {
		judgeWeight = 5
	}
	judgeAvailable := evaluation.JudgeScore != nil
	judge := model.EvaluationEvidence{Name: "blind_judge", Weight: judgeWeight, Available: judgeAvailable, Reference: evaluation.JudgeReason}
	if judgeAvailable {
		judge.Score = *evaluation.JudgeScore
		judge.Passed = *evaluation.JudgeScore >= 60
	}
	evaluation.Evidence = append(evaluation.Evidence, judge)
	recalculateEvaluation(&evaluation)
	var runCount, reported int
	var totalTokens int64
	if err = tx.QueryRowContext(ctx, `WITH RECURSIVE scope(task_id) AS(SELECT ? UNION SELECT e.to_task_id FROM task_edge e JOIN scope s ON e.from_task_id=s.task_id WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS')) SELECT COUNT(DISTINCT r.run_id),COUNT(DISTINCT u.run_id),COALESCE(SUM(u.input_tokens+u.output_tokens),0) FROM run r JOIN scope s ON s.task_id=r.task_id LEFT JOIN run_token_usage u ON u.run_id=r.run_id`, rootTaskID).Scan(&runCount, &reported, &totalTokens); err != nil {
		return evaluation, model.EvaluationInput{}, err
	}
	if runCount > 0 && reported == runCount {
		evaluation.TotalTokens = &totalTokens
	}
	var taskState string
	_ = tx.QueryRowContext(ctx, `SELECT state FROM task WHERE task_id=?`, rootTaskID).Scan(&taskState)
	evaluation.FailedSample = evaluation.FailedSample || (taskState == model.TaskStateBlocked && time.Now().UnixMilli()-summary.CompletedAtMS >= 24*time.Hour.Milliseconds())
	if evaluation.FailedSample {
		evaluation.QualityScore = 0
	}
	evaluation.Mature = evaluation.FailedSample || (time.Now().UnixMilli() >= evaluation.ObservationDueAtMS && evaluation.Coverage >= 70)
	artifacts, err := evaluationArtifactsTx(ctx, tx, summary.AcceptedArtifacts, 64*1024)
	if err != nil {
		return evaluation, model.EvaluationInput{}, err
	}
	input := model.EvaluationInput{TaskID: rootTaskID, TaskType: taskType, Title: summary.Title, Goal: summary.Goal, Result: summary.Result, Artifacts: artifacts, Evidence: append([]model.EvaluationEvidence{}, evaluation.Evidence[:len(evaluation.Evidence)-1]...)}
	return evaluation, input, nil
}

func evaluationArtifactsTx(ctx context.Context, tx *sql.Tx, accepted []model.TaskSummaryArtifact, budget int) ([]model.EvaluationArtifact, error) {
	items := make([]model.EvaluationArtifact, 0, len(accepted))
	remaining := budget
	for _, snapshot := range accepted {
		artifact, err := readJSONRow[model.Artifact](tx.QueryRowContext(ctx, `SELECT data_json FROM artifact WHERE artifact_id=?`, snapshot.ArtifactID))
		if err != nil {
			return nil, err
		}
		if artifact.Version != snapshot.Version || artifact.SHA256 != snapshot.SHA256 {
			return nil, errors.New("accepted artifact snapshot no longer matches persisted content")
		}
		content := artifact.Content
		truncated := false
		if len(content) > remaining {
			content = truncateUTF8Bytes(content, remaining)
			truncated = true
		}
		remaining -= len(content)
		items = append(items, model.EvaluationArtifact{ArtifactID: artifact.ID, Name: artifact.Name, Version: artifact.Version, SHA256: artifact.SHA256, Content: content, Truncated: truncated})
		if remaining <= 0 {
			remaining = 0
		}
	}
	return items, nil
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && end < len(value) && value[end]&0xc0 == 0x80 {
		end--
	}
	return value[:end]
}

func qualityEvidence(name string, weight float64, available, passed bool, reference string) model.EvaluationEvidence {
	score := 0.0
	if passed {
		score = 100
	}
	return model.EvaluationEvidence{Name: name, Weight: weight, Available: available, Passed: passed, Score: score, Reference: redactObservation(reference)}
}
func recalculateEvaluation(e *model.TaskEvaluation) {
	e.Coverage, e.QualityScore = 0, 0
	for _, item := range e.Evidence {
		if item.Available {
			e.Coverage += item.Weight
			e.QualityScore += item.Score * item.Weight / 100
		}
	}
}
func saveTaskEvaluationTx(ctx context.Context, tx *sql.Tx, e model.TaskEvaluation, eventType string) error {
	e.UpdatedAtMS = time.Now().UnixMilli()
	raw, _ := json.Marshal(e)
	var experimentID any
	if e.ExperimentID != "" {
		experimentID = e.ExperimentID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_evaluation(task_id,root_task_id,experiment_id,arm,mature,updated_at_ms,data_json) VALUES(?,?,?,?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET experiment_id=excluded.experiment_id,arm=excluded.arm,mature=excluded.mature,updated_at_ms=excluded.updated_at_ms,data_json=excluded.data_json`, e.TaskID, e.RootTaskID, experimentID, e.Arm, e.Mature, e.UpdatedAtMS, raw); err != nil {
		return err
	}
	_, err := appendEventTx(ctx, tx, "task_evaluation", e.ID, eventType, "", e.RootTaskID, map[string]any{"task_id": e.TaskID, "coverage": e.Coverage, "quality": e.QualityScore, "mature": e.Mature})
	return err
}

func applyJudgementTx(ctx context.Context, tx *sql.Tx, rootTaskID string, judgement model.SemanticJudgement) error {
	evaluation, _, err := prepareEvaluationTx(ctx, tx, rootTaskID)
	if err != nil {
		return err
	}
	evaluation.JudgeScore = &judgement.Score
	evaluation.JudgeReason = judgement.Reason
	for i := range evaluation.Evidence {
		if evaluation.Evidence[i].Name == "blind_judge" {
			evaluation.Evidence[i].Available = true
			evaluation.Evidence[i].Score = judgement.Score
			evaluation.Evidence[i].Passed = judgement.Score >= 60
			evaluation.Evidence[i].Reference = redactObservation(judgement.Reason)
		}
	}
	recalculateEvaluation(&evaluation)
	if evaluation.FailedSample {
		evaluation.QualityScore = 0
	}
	evaluation.Mature = evaluation.FailedSample || (time.Now().UnixMilli() >= evaluation.ObservationDueAtMS && evaluation.Coverage >= 70)
	if err = saveTaskEvaluationTx(ctx, tx, evaluation, "TaskEvaluationCompleted"); err != nil {
		return err
	}
	if evaluation.ExperimentID != "" {
		return evaluateExperimentTx(ctx, tx, evaluation.ExperimentID)
	}
	return nil
}

func evaluateExperimentTx(ctx context.Context, tx *sql.Tx, experimentID string) error {
	experiment, err := readJSONRow[model.ImprovementExperiment](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_experiment WHERE experiment_id=?`, experimentID))
	if err != nil {
		return err
	}
	items, err := listJSONRows[model.TaskEvaluation](ctx, tx, `SELECT data_json FROM task_evaluation WHERE experiment_id=? AND mature=1`, experimentID)
	if err != nil {
		return err
	}
	baseline, candidate, promoted := []model.TaskEvaluation{}, []model.TaskEvaluation{}, []model.TaskEvaluation{}
	for _, item := range items {
		switch item.Arm {
		case model.ExperimentArmBaseline:
			baseline = append(baseline, item)
		case model.ExperimentArmCandidate:
			candidate = append(candidate, item)
		case model.ExperimentArmPromoted:
			promoted = append(promoted, item)
		}
		if item.SafetyViolation && (item.Arm == model.ExperimentArmCandidate || item.Arm == model.ExperimentArmPromoted) {
			return transitionExperimentTx(ctx, tx, &experiment, model.CandidateStateRolledBack, "安全边界违规，立即回滚")
		}
	}
	experiment.BaselineSamples, experiment.CandidateSamples, experiment.PromotedSamples = len(baseline), len(candidate), len(promoted)
	if score, ok := improvement.CompositeScore(baseline, candidate); ok {
		experiment.Score = score
	}
	now := time.Now().UnixMilli()
	switch experiment.State {
	case model.CandidateStateCanary:
		if len(baseline) >= 3 && len(candidate) >= 3 && experiment.Score.Composite < -15 {
			return transitionExperimentTx(ctx, tx, &experiment, model.CandidateStateRejected, "至少三组样本后综合分低于 -15，提前停止")
		}
		if len(baseline) >= 5 && len(candidate) >= 5 && experiment.Score.Composite >= 8 {
			return transitionExperimentTx(ctx, tx, &experiment, model.CandidateStatePromoted, "每组至少五个成熟样本且综合改善达到 +8")
		}
		if (len(baseline) >= 15 && len(candidate) >= 15) || now-experiment.CreatedAtMS >= 30*24*time.Hour.Milliseconds() {
			return transitionExperimentTx(ctx, tx, &experiment, model.CandidateStateRejected, "达到样本或 30 天上限，综合改善未达到 +8")
		}
	case model.CandidateStatePromoted:
		if len(promoted) >= 10 {
			postScore, ok := improvement.CompositeScore(baseline, promoted)
			if ok && postScore.Composite < -8 {
				return transitionExperimentTx(ctx, tx, &experiment, model.CandidateStateRolledBack, "推广后十个成熟任务低于原基线 -8")
			}
			if ok {
				// Once the post-promotion observation window closes, the score
				// shown on the experiment and on the promoted catalog entry must
				// describe realized production results, not the earlier canary.
				experiment.Score = postScore
			}
			experiment.State = model.ExperimentStateStable
			experiment.DecisionReason = "推广后十个成熟任务未触发退化回滚，持续监测完成"
			experiment.UpdatedAtMS = now
			candidateRecord, readErr := readJSONRow[model.ImprovementCandidate](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_candidate WHERE candidate_id=?`, experiment.CandidateID))
			if readErr != nil {
				return readErr
			}
			if experiment.CandidateExperienceID != "" {
				current, experienceErr := readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience WHERE experience_id=?`, experiment.CandidateExperienceID))
				if experienceErr != nil {
					return experienceErr
				}
				if _, experienceErr = publishExperienceRevisionTx(ctx, tx, current, current, current.State, candidateRecord, experiment, "ExperienceEffectStabilized"); experienceErr != nil {
					return experienceErr
				}
			}
			if experiment.CandidatePolicyID != "" {
				if _, err = appendEventTx(ctx, tx, "optimization_policy", experiment.CandidatePolicyID, "OptimizationPolicyStabilized", experiment.CandidateID, candidateRecord.SourceTaskID, map[string]any{"state": "ACTIVE", "reason": experiment.DecisionReason, "score": experiment.Score, "samples": experiment.CandidateSamples + experiment.PromotedSamples}); err != nil {
					return err
				}
			}
			raw, _ := json.Marshal(experiment)
			if _, err = tx.ExecContext(ctx, `UPDATE improvement_experiment SET state=?,updated_at_ms=?,data_json=? WHERE experiment_id=?`, experiment.State, now, raw, experiment.ID); err != nil {
				return err
			}
			_, err = appendEventTx(ctx, tx, "improvement_experiment", experiment.ID, "ImprovementExperimentStabilized", experiment.CandidateID, experiment.CandidateID, map[string]any{"score": experiment.Score, "samples": len(promoted)})
			return err
		}
	}
	experiment.UpdatedAtMS = now
	raw, _ := json.Marshal(experiment)
	if _, err = tx.ExecContext(ctx, `UPDATE improvement_experiment SET updated_at_ms=?,data_json=? WHERE experiment_id=?`, now, raw, experiment.ID); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "improvement_experiment", experiment.ID, "ImprovementExperimentEvaluated", experiment.CandidateID, experiment.CandidateID, map[string]any{"baseline_samples": experiment.BaselineSamples, "candidate_samples": experiment.CandidateSamples, "promoted_samples": experiment.PromotedSamples, "score": experiment.Score})
	return err
}

func transitionExperimentTx(ctx context.Context, tx *sql.Tx, experiment *model.ImprovementExperiment, state, reason string) error {
	now := time.Now().UnixMilli()
	experiment.State, experiment.DecisionReason, experiment.UpdatedAtMS = state, reason, now
	if state == model.CandidateStatePromoted {
		experiment.PromotedAtMS = now
	}
	candidate, err := readJSONRow[model.ImprovementCandidate](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_candidate WHERE candidate_id=?`, experiment.CandidateID))
	if err != nil {
		return err
	}
	candidate.State, candidate.Version, candidate.UpdatedAtMS = state, candidate.Version+1, now
	if err = saveCandidateTx(ctx, tx, candidate, "ImprovementCandidate"+state); err != nil {
		return err
	}
	policyState := "RETIRED"
	experienceState := "RETIRED"
	if state == model.CandidateStateCanary {
		policyState = "CANARY"
		experienceState = "CANARY"
	} else if state == model.CandidateStatePaused {
		policyState = "PAUSED"
		experienceState = "PAUSED"
	} else if state == model.CandidateStatePromoted {
		policyState = "ACTIVE"
		experienceState = "ACTIVE"
	} else if state == model.CandidateStateRolledBack {
		policyState = "ROLLED_BACK"
		experienceState = "ROLLED_BACK"
	}
	if experiment.CandidatePolicyID != "" {
		var policy model.OptimizationPolicy
		policy, err = getPolicyTx(ctx, tx, experiment.CandidatePolicyID, experiment.CandidatePolicyVersion)
		if err != nil {
			return err
		}
		policy.State = policyState
		raw, _ := json.Marshal(policy)
		if _, err = tx.ExecContext(ctx, `UPDATE optimization_policy SET state=?,data_json=? WHERE policy_id=? AND version=?`, policy.State, raw, policy.ID, policy.Version); err != nil {
			return err
		}
		if _, err = appendEventTx(ctx, tx, "optimization_policy", policy.ID, "OptimizationPolicyStateChanged", candidate.ID, candidate.SourceTaskID, map[string]any{"state": policy.State, "version": policy.Version, "reason": reason}); err != nil {
			return err
		}
	}
	if experiment.CandidateExperienceID != "" {
		var current model.Experience
		current, err = readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience WHERE experience_id=?`, experiment.CandidateExperienceID))
		if err != nil {
			return err
		}
		operation := experiment.ExperienceOperation
		if operation == "" {
			operation = "add"
		}
		switch operation {
		case "add":
			if _, err = publishExperienceRevisionTx(ctx, tx, current, current, experienceState, candidate, *experiment, "ExperienceStateChanged"); err != nil {
				return err
			}
		case "revise":
			if state == model.CandidateStatePromoted {
				staged, readErr := readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience_revision WHERE experience_id=? AND revision=?`, current.ID, experiment.CandidateExperienceRev))
				if readErr != nil {
					return readErr
				}
				if _, err = publishExperienceRevisionTx(ctx, tx, current, staged, "ACTIVE", candidate, *experiment, "ExperienceRevisionPromoted"); err != nil {
					return err
				}
			} else if state == model.CandidateStateRolledBack && experiment.PromotedAtMS > 0 {
				baseline, readErr := readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience_revision WHERE experience_id=? AND revision=?`, current.ID, experiment.BaselineExperienceRev))
				if readErr != nil {
					return readErr
				}
				if _, err = publishExperienceRevisionTx(ctx, tx, current, baseline, "ACTIVE", candidate, *experiment, "ExperienceRevisionRolledBack"); err != nil {
					return err
				}
			} else if _, err = appendEventTx(ctx, tx, "experience", current.ID, "ExperienceRevisionCandidateStateChanged", candidate.ID, candidate.SourceTaskID, map[string]any{"state": state, "candidate_revision": experiment.CandidateExperienceRev, "reason": reason}); err != nil {
				return err
			}
		case "retire":
			if state == model.CandidateStatePromoted {
				if _, err = publishExperienceRevisionTx(ctx, tx, current, current, "RETIRED", candidate, *experiment, "ExperienceRetirementPromoted"); err != nil {
					return err
				}
			} else if state == model.CandidateStateRolledBack && experiment.PromotedAtMS > 0 {
				baseline, readErr := readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience_revision WHERE experience_id=? AND revision=?`, current.ID, experiment.BaselineExperienceRev))
				if readErr != nil {
					return readErr
				}
				if _, err = publishExperienceRevisionTx(ctx, tx, current, baseline, "ACTIVE", candidate, *experiment, "ExperienceRevisionRolledBack"); err != nil {
					return err
				}
			} else if _, err = appendEventTx(ctx, tx, "experience", current.ID, "ExperienceRetirementCandidateStateChanged", candidate.ID, candidate.SourceTaskID, map[string]any{"state": state, "reason": reason}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: invalid experience operation", model.ErrValidation)
		}
	}
	raw, _ := json.Marshal(experiment)
	if _, err = tx.ExecContext(ctx, `UPDATE improvement_experiment SET state=?,updated_at_ms=?,data_json=? WHERE experiment_id=?`, state, now, raw, experiment.ID); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "improvement_experiment", experiment.ID, "ImprovementExperiment"+state, candidate.ID, candidate.SourceTaskID, map[string]any{"reason": reason, "score": experiment.Score})
	return err
}

func (s *Store) ActOnImprovementCandidate(ctx context.Context, candidateID, action, reason, key string, expected int64, extraLockFields ...[]string) (model.ImprovementCandidate, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 2000 {
		return model.ImprovementCandidate{}, fmt.Errorf("%w: audit reason required", model.ErrValidation)
	}
	if strings.TrimSpace(key) == "" || len(key) > 240 {
		return model.ImprovementCandidate{}, fmt.Errorf("%w: idempotency key required", model.ErrValidation)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ImprovementCandidate{}, err
	}
	defer tx.Rollback()
	if key != "" {
		var resource string
		if err = tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope=? AND key=?`, "improvement.candidate:"+candidateID, key).Scan(&resource); err == nil {
			return readJSONRow[model.ImprovementCandidate](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_candidate WHERE candidate_id=?`, candidateID))
		}
		if err != sql.ErrNoRows {
			return model.ImprovementCandidate{}, err
		}
	}
	candidate, err := readJSONRow[model.ImprovementCandidate](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_candidate WHERE candidate_id=?`, candidateID))
	if err != nil {
		return candidate, err
	}
	if candidate.Version != expected {
		return candidate, fmt.Errorf("%w: candidate changed", model.ErrConflict)
	}
	if action == "unlock_fields" {
		if len(candidate.LockedFields) == 0 {
			return candidate, fmt.Errorf("%w: candidate has no operator field lock", model.ErrConflict)
		}
		candidate.LockedFields = nil
		candidate.Version++
		candidate.UpdatedAtMS = time.Now().UnixMilli()
		err = saveCandidateTx(ctx, tx, candidate, "ImprovementCandidateFieldsUnlocked")
	} else {
		effectiveAction := action
		if action == "reject_and_lock" {
			changedFields, changeErr := changedPolicyFieldsTx(ctx, tx, candidate)
			if changeErr != nil {
				return candidate, changeErr
			}
			requested := []string{}
			if len(extraLockFields) > 0 {
				requested = extraLockFields[0]
			}
			candidate.LockedFields, err = policyLockFields(candidate.Type, append(changedFields, requested...))
			if err != nil {
				return candidate, err
			}
			candidate.UpdatedAtMS = time.Now().UnixMilli()
			// Persist the lock before transitionExperimentTx reloads the
			// candidate. The enclosing transaction makes both changes atomic.
			if err = saveCandidateTx(ctx, tx, candidate, "ImprovementCandidateFieldsLocked"); err != nil {
				return candidate, err
			}
			effectiveAction = "reject"
		}
		if candidate.ExperimentID != "" && (effectiveAction == "promote" || effectiveAction == "reject" || effectiveAction == "rollback" || effectiveAction == "pause" || (effectiveAction == "start" && candidate.State == model.CandidateStatePaused)) {
			experiment, readErr := readJSONRow[model.ImprovementExperiment](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_experiment WHERE experiment_id=?`, candidate.ExperimentID))
			if readErr != nil {
				return candidate, readErr
			}
			validTransition := (experiment.State == model.CandidateStateCanary && (effectiveAction == "promote" || effectiveAction == "pause" || effectiveAction == "reject")) ||
				(experiment.State == model.CandidateStatePaused && (effectiveAction == "start" || effectiveAction == "reject")) ||
				((experiment.State == model.CandidateStatePromoted || experiment.State == model.ExperimentStateStable) && effectiveAction == "rollback")
			if !validTransition {
				return candidate, fmt.Errorf("%w: action is not valid in current state", model.ErrConflict)
			}
			state := map[string]string{"promote": model.CandidateStatePromoted, "reject": model.CandidateStateRejected, "rollback": model.CandidateStateRolledBack, "pause": model.CandidateStatePaused, "start": model.CandidateStateCanary}[effectiveAction]
			if state == "" {
				return candidate, fmt.Errorf("%w: invalid action", model.ErrValidation)
			}
			if state == model.CandidateStateCanary {
				readiness, readinessErr := improvementReadinessTx(ctx, tx)
				if readinessErr != nil {
					return candidate, readinessErr
				}
				if !readiness.Ready {
					return candidate, fmt.Errorf("%w: %s", model.ErrConflict, readiness.Reason)
				}
				active, listErr := listJSONRows[model.ImprovementExperiment](ctx, tx, `SELECT data_json FROM improvement_experiment WHERE state IN ('CANARY','PROMOTED') AND experiment_id<>?`, experiment.ID)
				if listErr != nil {
					return candidate, listErr
				}
				for _, other := range active {
					if scopesOverlap(experiment.Scope, other.Scope) {
						return candidate, fmt.Errorf("%w: one matching experiment is already active", model.ErrConflict)
					}
				}
			}
			if err = transitionExperimentTx(ctx, tx, &experiment, state, "人工操作："+reason); err != nil {
				return candidate, err
			}
			candidate, err = readJSONRow[model.ImprovementCandidate](tx.QueryRowContext(ctx, `SELECT data_json FROM improvement_candidate WHERE candidate_id=?`, candidateID))
		} else if effectiveAction == "start" && candidate.State == model.CandidateStateValidated {
			readiness, readinessErr := improvementReadinessTx(ctx, tx)
			if readinessErr != nil {
				return candidate, readinessErr
			}
			if !readiness.Ready {
				return candidate, fmt.Errorf("%w: %s", model.ErrConflict, readiness.Reason)
			}
			candidate, err = activateCandidateTx(ctx, tx, candidate)
		} else if effectiveAction == "reject" && candidate.State == model.CandidateStateValidated {
			candidate.State, candidate.ValidationError = model.CandidateStateRejected, "人工拒绝："+reason
			candidate.Version++
			candidate.UpdatedAtMS = time.Now().UnixMilli()
			err = saveCandidateTx(ctx, tx, candidate, "ImprovementCandidateRejected")
		} else {
			return candidate, fmt.Errorf("%w: action is not valid in current state", model.ErrConflict)
		}
		if err == nil && action == "reject_and_lock" {
			err = rejectCandidatesCoveredByLockTx(ctx, tx, candidate)
		}
	}
	if err != nil {
		return candidate, err
	}
	if key != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_key VALUES(?,?,?,?)`, "improvement.candidate:"+candidateID, key, candidateID, time.Now().UnixMilli()); err != nil {
			return candidate, err
		}
	}
	if _, err = appendEventTx(ctx, tx, "improvement_candidate", candidate.ID, "ImprovementCandidateAction", "", candidate.SourceTaskID, map[string]any{"action": action, "reason": reason, "expected_version": expected}); err != nil {
		return candidate, err
	}
	return candidate, tx.Commit()
}

func (s *Store) ActOnExperience(ctx context.Context, experienceID, action, reason, key string, expected int64) (model.Experience, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 2000 {
		return model.Experience{}, fmt.Errorf("%w: audit reason required", model.ErrValidation)
	}
	if strings.TrimSpace(key) == "" || len(key) > 240 {
		return model.Experience{}, fmt.Errorf("%w: idempotency key required", model.ErrValidation)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Experience{}, err
	}
	defer tx.Rollback()
	if key != "" {
		var resource string
		if err = tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope=? AND key=?`, "improvement.experience:"+experienceID, key).Scan(&resource); err == nil {
			return readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience WHERE experience_id=?`, experienceID))
		} else if err != sql.ErrNoRows {
			return model.Experience{}, err
		}
	}
	item, err := readJSONRow[model.Experience](tx.QueryRowContext(ctx, `SELECT data_json FROM experience WHERE experience_id=?`, experienceID))
	if err != nil {
		return item, err
	}
	if item.Version != expected {
		return item, fmt.Errorf("%w: experience changed", model.ErrConflict)
	}
	states := map[string]string{"activate": "ACTIVE", "pause": "PAUSED", "retire": "RETIRED"}
	state := states[action]
	if state == "" {
		return item, fmt.Errorf("%w: invalid experience action", model.ErrValidation)
	}
	revision, err := nextExperienceRevisionTx(ctx, tx, item.ID)
	if err != nil {
		return item, err
	}
	generation, err := nextExperienceGenerationTx(ctx, tx)
	if err != nil {
		return item, err
	}
	item.State, item.Revision, item.Generation, item.Version, item.UpdatedAtMS = state, revision, generation, item.Version+1, time.Now().UnixMilli()
	if err = insertExperienceRevisionTx(ctx, tx, item); err != nil {
		return item, err
	}
	raw, _ := json.Marshal(item)
	if _, err = tx.ExecContext(ctx, `UPDATE experience SET current_revision=?,state=?,generation=?,version=?,updated_at_ms=?,data_json=? WHERE experience_id=?`, item.Revision, item.State, item.Generation, item.Version, item.UpdatedAtMS, raw, item.ID); err != nil {
		return item, err
	}
	if _, err = appendEventTx(ctx, tx, "experience", item.ID, "ExperienceStateChanged", "", item.ID, map[string]any{"action": action, "reason": reason, "version": item.Version}); err != nil {
		return item, err
	}
	if key != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_key VALUES(?,?,?,?)`, "improvement.experience:"+experienceID, key, experienceID, time.Now().UnixMilli()); err != nil {
			return item, err
		}
	}
	return item, tx.Commit()
}

func (s *Store) ImprovementOverview(ctx context.Context) (model.ImprovementOverview, error) {
	var result model.ImprovementOverview
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	result.Readiness, err = improvementReadinessTx(ctx, tx)
	if err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM improvement_experiment WHERE state IN ('CANARY','PROMOTED')),(SELECT COUNT(*) FROM improvement_candidate WHERE state IN ('DISCOVERED','VALIDATED','CANARY')),(SELECT COUNT(*) FROM experience WHERE state IN ('ACTIVE','CANARY')),(SELECT COUNT(*) FROM improvement_job WHERE state='FAILED')`).Scan(&result.ActiveExperiments, &result.PendingCandidates, &result.ExperienceCount, &result.FailedJobs); err != nil {
		return result, err
	}
	evaluations, err := listJSONRows[model.TaskEvaluation](ctx, tx, `SELECT data_json FROM task_evaluation WHERE mature=1`)
	if err != nil {
		return result, err
	}
	if len(evaluations) > 0 {
		var quality float64
		cycles := make([]int64, 0, len(evaluations))
		human := make([]int, 0, len(evaluations))
		for _, evaluation := range evaluations {
			quality += evaluation.QualityScore
			cycles = append(cycles, evaluation.CycleTimeMS)
			human = append(human, evaluation.HumanInterventions)
		}
		result.MatureEvaluations = len(evaluations)
		result.AverageQuality = math.Round(quality/float64(len(evaluations))*100) / 100
		result.MedianCycleTimeMS = improvementMedianInt64(cycles)
		result.MedianHumanInput = improvementMedianInt(human)
	}
	var improvementTokens int64
	var incompleteImprovementRuns int
	if err = tx.QueryRowContext(ctx, `SELECT
		(SELECT COALESCE(SUM(u.input_tokens+u.output_tokens),0) FROM run_token_usage u JOIN improvement_job j ON j.run_id=u.run_id),
		(SELECT COUNT(*) FROM improvement_job j JOIN run r ON r.run_id=j.run_id
		 WHERE r.state NOT IN ('QUEUED','RUNNING') AND NOT EXISTS(SELECT 1 FROM run_token_usage u WHERE u.run_id=r.run_id))`).Scan(&improvementTokens, &incompleteImprovementRuns); err != nil {
		return result, err
	}
	if incompleteImprovementRuns == 0 {
		result.ImprovementTokens = &improvementTokens
	}
	var businessTokens int64
	var unreported int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(u.input_tokens+u.output_tokens),0),(SELECT COUNT(*) FROM run r WHERE r.state NOT IN ('QUEUED','RUNNING') AND NOT EXISTS(SELECT 1 FROM improvement_job j WHERE j.run_id=r.run_id) AND NOT EXISTS(SELECT 1 FROM run_token_usage x WHERE x.run_id=r.run_id)) FROM run_token_usage u WHERE NOT EXISTS(SELECT 1 FROM improvement_job j WHERE j.run_id=u.run_id)`).Scan(&businessTokens, &unreported); err != nil {
		return result, err
	}
	if unreported == 0 {
		result.BusinessTokens = &businessTokens
	}
	// Estimate realized gross saving from each experiment's actual mature
	// baseline and non-baseline samples. Never multiply a global token total by
	// a percentage from one experiment: tasks outside that experiment are not a
	// valid counterfactual. If one participating sample lacks token telemetry,
	// the aggregate is unknown rather than silently treating the sample as zero.
	experiments, err := listJSONRows[model.ImprovementExperiment](ctx, tx, `SELECT data_json FROM improvement_experiment ORDER BY created_at_ms`)
	if err != nil {
		return result, err
	}
	gross, grossKnown := int64(0), true
	for _, experiment := range experiments {
		evaluations, listErr := listJSONRows[model.TaskEvaluation](ctx, tx, `SELECT data_json FROM task_evaluation WHERE experiment_id=? AND mature=1`, experiment.ID)
		if listErr != nil {
			return result, listErr
		}
		baselineTokens := []int64{}
		candidateTokens := []int64{}
		promotedTokens := []int64{}
		baselineCount, candidateCount, promotedCount := 0, 0, 0
		for _, evaluation := range evaluations {
			switch evaluation.Arm {
			case model.ExperimentArmBaseline:
				baselineCount++
				if evaluation.TotalTokens != nil {
					baselineTokens = append(baselineTokens, *evaluation.TotalTokens)
				}
			case model.ExperimentArmCandidate:
				candidateCount++
				if evaluation.TotalTokens != nil {
					candidateTokens = append(candidateTokens, *evaluation.TotalTokens)
				}
			case model.ExperimentArmPromoted:
				promotedCount++
				if evaluation.TotalTokens != nil {
					promotedTokens = append(promotedTokens, *evaluation.TotalTokens)
				}
			}
		}
		if candidateCount+promotedCount == 0 {
			continue
		}
		if baselineCount == 0 || len(baselineTokens) != baselineCount || len(candidateTokens) != candidateCount || len(promotedTokens) != promotedCount {
			grossKnown = false
			continue
		}
		baselineMedian := improvementMedianInt64(baselineTokens)
		if candidateCount > 0 {
			gross += (baselineMedian - improvementMedianInt64(candidateTokens)) * int64(candidateCount)
		}
		if promotedCount > 0 {
			gross += (baselineMedian - improvementMedianInt64(promotedTokens)) * int64(promotedCount)
		}
	}
	if grossKnown {
		result.GrossTokenSaving = &gross
		if result.ImprovementTokens != nil {
			net := gross - *result.ImprovementTokens
			result.NetTokenSaving = &net
		}
	}
	if result.FailedJobs > 0 {
		result.NeedsAttention = append(result.NeedsAttention, fmt.Sprintf("%d 个改进后台任务失败", result.FailedJobs))
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM improvement_experiment WHERE state='ROLLED_BACK' AND updated_at_ms>=?`, time.Now().Add(-7*24*time.Hour).UnixMilli()).Scan(&result.RecentRollbacks); err != nil {
		return result, err
	}
	if result.RecentRollbacks > 0 {
		result.NeedsAttention = append(result.NeedsAttention, fmt.Sprintf("最近 7 天有 %d 个改进实验自动回滚", result.RecentRollbacks))
	}
	var projectorRaw []byte
	if err = tx.QueryRowContext(ctx, `SELECT value FROM improvement_meta WHERE key='projector:last_error'`).Scan(&projectorRaw); err == nil {
		var projection struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(projectorRaw, &projection) == nil {
			result.ProjectorError = projection.Error
			result.NeedsAttention = append(result.NeedsAttention, "改进观测投影失败，后台将自动补偿")
		}
	} else if err != sql.ErrNoRows {
		return result, err
	}
	return result, nil
}

func improvementMedianInt64(values []int64) int64 {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	if len(values) == 0 {
		return 0
	}
	mid := len(values) / 2
	if len(values)%2 == 0 {
		return (values[mid-1] + values[mid]) / 2
	}
	return values[mid]
}

func improvementMedianInt(values []int) int {
	sort.Ints(values)
	if len(values) == 0 {
		return 0
	}
	mid := len(values) / 2
	if len(values)%2 == 0 {
		return (values[mid-1] + values[mid]) / 2
	}
	return values[mid]
}
