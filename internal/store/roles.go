package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
	"work-assistant/internal/rolebuilder"
	"work-assistant/internal/router"
)

func migrateV4(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 4`).Scan(&version); err != nil {
		return err
	}
	if version > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE role(role_id TEXT PRIMARY KEY, data_json TEXT NOT NULL)`,
		`CREATE TABLE role_draft(draft_id TEXT PRIMARY KEY, task_id TEXT NOT NULL UNIQUE REFERENCES task(task_id), data_json TEXT NOT NULL)`,
		`CREATE TABLE agent_profile(agent_id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, role_id TEXT NOT NULL REFERENCES role(role_id), runtime_id TEXT NOT NULL REFERENCES runtime(runtime_id), data_json TEXT NOT NULL)`,
		`CREATE INDEX agent_profile_role_idx ON agent_profile(role_id)`,
		`CREATE INDEX run_agent_state_idx ON run(agent_id, state)`,
		`ALTER TABLE task ADD COLUMN requirements_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE run ADD COLUMN role_snapshot_json TEXT NOT NULL DEFAULT 'null'`,
		`ALTER TABLE run ADD COLUMN output TEXT NOT NULL DEFAULT ''`,
		`INSERT INTO schema_version(version, applied_at_ms) VALUES(4, unixepoch('subsec') * 1000)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate v4: %w", err)
		}
	}
	return tx.Commit()
}

func readJSONRow[T any](row rowScanner) (T, error) {
	var result T
	var data []byte
	if err := row.Scan(&data); err != nil {
		return result, err
	}
	err := json.Unmarshal(data, &result)
	return result, err
}

func listJSONRows[T any](ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []T{}
	for rows.Next() {
		item, err := readJSONRow[T](rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ListRoles(ctx context.Context) ([]model.Role, error) {
	return listJSONRows[model.Role](ctx, s.db, `SELECT data_json FROM role ORDER BY role_id`)
}

func (s *Store) GetRunByKey(ctx context.Context, taskID, key string) (model.Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE run_id = (SELECT resource_id FROM idempotency_key WHERE scope = ? AND key = ?)`, "task.run:"+taskID, key))
}
func (s *Store) GetRole(ctx context.Context, roleID string) (model.Role, error) {
	return readJSONRow[model.Role](s.db.QueryRowContext(ctx, `SELECT data_json FROM role WHERE role_id = ?`, roleID))
}
func (s *Store) ListRoleDrafts(ctx context.Context) ([]model.RoleDraft, error) {
	return listJSONRows[model.RoleDraft](ctx, s.db, `SELECT data_json FROM role_draft ORDER BY rowid DESC LIMIT 100`)
}
func (s *Store) GetRoleDraft(ctx context.Context, draftID string) (model.RoleDraft, error) {
	return readJSONRow[model.RoleDraft](s.db.QueryRowContext(ctx, `SELECT data_json FROM role_draft WHERE draft_id = ?`, draftID))
}
func getRoleDraftTx(ctx context.Context, tx *sql.Tx, draftID string) (model.RoleDraft, error) {
	return readJSONRow[model.RoleDraft](tx.QueryRowContext(ctx, `SELECT data_json FROM role_draft WHERE draft_id = ?`, draftID))
}

func (s *Store) CreateRoleDraft(ctx context.Context, description, sourceRoleID, key string) (model.RoleDraft, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.RoleDraft{}, err
	}
	defer tx.Rollback()
	draft, err := createRoleDraftTx(ctx, tx, description, sourceRoleID, key)
	if err != nil {
		return draft, err
	}
	return draft, tx.Commit()
}

func createRoleDraftTx(ctx context.Context, tx *sql.Tx, description, sourceRoleID, key string) (model.RoleDraft, error) {
	if len(description) > 4000 {
		return model.RoleDraft{}, fmt.Errorf("%w: description exceeds 4000 bytes", model.ErrValidation)
	}
	if key != "" {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope = 'role.draft' AND key = ?`, key).Scan(&existing)
		if err == nil {
			return getRoleDraftTx(ctx, tx, existing)
		}
		if err != sql.ErrNoRows {
			return model.RoleDraft{}, err
		}
	}
	now := time.Now().UTC().UnixMilli()
	draft := model.RoleDraft{ID: id.New("draft"), State: "DRAFT", Version: 1,
		Spec:     model.RoleSpec{Description: description, Capabilities: []string{}, Boundaries: []string{}},
		Messages: []model.RoleMessage{}, Questions: []string{}, CreatedAtMS: now, UpdatedAtMS: now}
	if sourceRoleID != "" {
		role, err := readJSONRow[model.Role](tx.QueryRowContext(ctx, `SELECT data_json FROM role WHERE role_id = ?`, sourceRoleID))
		if err != nil {
			return model.RoleDraft{}, err
		}
		draft.Spec = role.RoleSpec
	}
	task, _, err := createTaskTx(ctx, tx, "", "Design role "+draft.ID, "Design a reusable role configuration. Do not execute the role's work or publish it.")
	if err != nil {
		return model.RoleDraft{}, err
	}
	draft.TaskID = task.ID
	data, _ := json.Marshal(draft)
	if _, err := tx.ExecContext(ctx, `INSERT INTO role_draft(draft_id, task_id, data_json) VALUES(?, ?, ?)`, draft.ID, draft.TaskID, data); err != nil {
		return model.RoleDraft{}, err
	}
	if _, err := appendEventTx(ctx, tx, "role_draft", draft.ID, "RoleDraftCreated", "", task.ID, draft); err != nil {
		return model.RoleDraft{}, err
	}
	if key != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_key(scope, key, resource_id, created_at_ms) VALUES('role.draft', ?, ?, ?)`, key, draft.ID, now); err != nil {
			return model.RoleDraft{}, err
		}
	}
	return draft, nil
}

func checkDraftEditable(draft model.RoleDraft, expected int64) error {
	if expected != draft.Version {
		return fmt.Errorf("%w: draft version changed; reload before editing", model.ErrConflict)
	}
	if draft.State == "GENERATING" || draft.State == "PUBLISHED" {
		return fmt.Errorf("%w: draft is %s", model.ErrConflict, draft.State)
	}
	return nil
}

func saveDraftTx(ctx context.Context, tx *sql.Tx, draft *model.RoleDraft, eventType string) error {
	draft.Version++
	draft.UpdatedAtMS = time.Now().UTC().UnixMilli()
	data, err := json.Marshal(draft)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE role_draft SET data_json = ? WHERE draft_id = ?`, data, draft.ID); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "role_draft", draft.ID, eventType, draft.LastRunID, draft.TaskID, draft)
	return err
}

func (s *Store) UpdateRoleDraft(ctx context.Context, draftID string, expected int64, spec model.RoleSpec) (model.RoleDraft, error) {
	if err := spec.Validate(false); err != nil {
		return model.RoleDraft{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.RoleDraft{}, err
	}
	defer tx.Rollback()
	draft, err := getRoleDraftTx(ctx, tx, draftID)
	if err != nil {
		return draft, err
	}
	if err := checkDraftEditable(draft, expected); err != nil {
		return draft, err
	}
	draft.Spec, draft.State, draft.Error = spec, "DRAFT", ""
	if err := saveDraftTx(ctx, tx, &draft, "RoleDraftEdited"); err != nil {
		return draft, err
	}
	return draft, tx.Commit()
}

// StartRoleDraftRun commits user input, run.start outbox and draft state together.
// A retried HTTP request with the same key cannot create a second AI turn.
func (s *Store) StartRoleDraftRun(ctx context.Context, draftID string, expected int64, message string, request CreateRunRequest, builder rolebuilder.Builder) (model.RoleDraft, model.Run, error) {
	message = strings.TrimSpace(message)
	if message == "" || len(message) > 16000 {
		return model.RoleDraft{}, model.Run{}, fmt.Errorf("%w: message is required and must be at most 16000 bytes", model.ErrValidation)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.RoleDraft{}, model.Run{}, err
	}
	defer tx.Rollback()
	draft, err := getRoleDraftTx(ctx, tx, draftID)
	if err != nil {
		return draft, model.Run{}, err
	}
	if request.IdempotencyKey != "" {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope = ? AND key = ?`, "task.run:"+draft.TaskID, request.IdempotencyKey).Scan(&existing)
		if err == nil {
			run, err := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE run_id = ?`, existing))
			return draft, run, err
		}
		if err != sql.ErrNoRows {
			return draft, model.Run{}, err
		}
	}
	if err := checkDraftEditable(draft, expected); err != nil {
		return draft, model.Run{}, err
	}
	if len(draft.Messages) >= 100 {
		return draft, model.Run{}, fmt.Errorf("%w: draft reached 50 exchanges; publish or duplicate it", model.ErrConflict)
	}
	request.TaskID, request.RoleDraftID = draft.TaskID, draft.ID
	request.ReadOnly = true
	request.Instructions, request.OutputSchema, request.Command = builder.Prompt(draft, message), builder.Schema(), nil
	run, err := createRunTx(ctx, tx, request)
	if err != nil {
		return draft, run, err
	}
	draft.State, draft.LastRunID, draft.Error = "GENERATING", run.ID, ""
	draft.Messages = append(draft.Messages, model.RoleMessage{Speaker: "user", Content: message, RunID: run.ID, CreatedAtMS: run.CreatedAtMS})
	if err := saveDraftTx(ctx, tx, &draft, "RoleDraftGenerationRequested"); err != nil {
		return draft, run, err
	}
	return draft, run, tx.Commit()
}

func applyRoleDraftResultTx(ctx context.Context, tx *sql.Tx, event model.RuntimeEvent, now int64) error {
	draft, err := readJSONRow[model.RoleDraft](tx.QueryRowContext(ctx, `SELECT d.data_json FROM role_draft d JOIN run r ON r.task_id = d.task_id WHERE r.run_id = ?`, event.RunID))
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if draft.LastRunID != event.RunID || draft.State != "GENERATING" {
		return nil
	}
	draft.State, draft.Error = "FAILED", event.Error
	if event.Type == "run.completed" {
		reply, err := (rolebuilder.JSONBuilder{}).Parse(event.Output)
		if err != nil {
			draft.Error = err.Error()
		} else {
			draft.State, draft.Error, draft.Spec, draft.Questions = "DRAFT", "", reply.Draft, reply.Questions
			draft.Messages = append(draft.Messages, model.RoleMessage{Speaker: "assistant", Content: reply.Message, RunID: event.RunID, CreatedAtMS: now})
		}
	}
	if draft.State == "FAILED" && draft.Error == "" {
		draft.Error = event.Type
	}
	return saveDraftTx(ctx, tx, &draft, "RoleDraftGenerationFinished")
}

func (s *Store) PublishRoleDraft(ctx context.Context, draftID string, expected int64) (model.Role, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Role{}, err
	}
	defer tx.Rollback()
	draft, err := getRoleDraftTx(ctx, tx, draftID)
	if err != nil {
		return model.Role{}, err
	}
	if draft.State == "PUBLISHED" {
		return readJSONRow[model.Role](tx.QueryRowContext(ctx, `SELECT data_json FROM role WHERE role_id = ?`, draft.PublishedRoleID))
	}
	if err := checkDraftEditable(draft, expected); err != nil {
		return model.Role{}, err
	}
	if err := draft.Spec.Validate(true); err != nil {
		return model.Role{}, err
	}
	role := model.Role{ID: id.New("role"), RoleSpec: draft.Spec, Version: 1, CreatedAtMS: time.Now().UTC().UnixMilli()}
	data, _ := json.Marshal(role)
	if _, err := tx.ExecContext(ctx, `INSERT INTO role(role_id, data_json) VALUES(?, ?)`, role.ID, data); err != nil {
		return model.Role{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO role_revision VALUES(?,?,?)`, role.ID, role.Version, data); err != nil {
		return model.Role{}, err
	}
	draft.State, draft.PublishedRoleID = "PUBLISHED", role.ID
	if err := saveDraftTx(ctx, tx, &draft, "RoleDraftPublished"); err != nil {
		return model.Role{}, err
	}
	if _, err := appendEventTx(ctx, tx, "role", role.ID, "RolePublished", draft.ID, draft.TaskID, role); err != nil {
		return model.Role{}, err
	}
	return role, tx.Commit()
}

func (s *Store) ListAgents(ctx context.Context) ([]model.AgentProfile, error) {
	rows, err := s.db.QueryContext(ctx, agentSelect+` ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []model.AgentProfile{}
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}
func (s *Store) GetAgent(ctx context.Context, agentID string) (model.AgentProfile, error) {
	return scanAgent(s.db.QueryRowContext(ctx, agentSelect+` WHERE a.agent_id = ?`, agentID))
}

const agentSelect = `SELECT a.data_json, r.data_json, (SELECT COUNT(*) FROM run WHERE agent_id = a.agent_id AND state IN ('QUEUED','RUNNING')) + (SELECT COUNT(*) FROM upgrade_job WHERE state IN ('QUEUED','BUILDING') AND json_extract(data_json,'$.builder.agent_id')=a.agent_id) FROM agent_profile a JOIN role r ON r.role_id = a.role_id`

func listAgentsTx(ctx context.Context, tx *sql.Tx) ([]model.AgentProfile, error) {
	rows, err := tx.QueryContext(ctx, agentSelect+` ORDER BY a.agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []model.AgentProfile{}
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func scanAgent(row rowScanner) (model.AgentProfile, error) {
	var agent model.AgentProfile
	var data, roleData []byte
	var active int
	if err := row.Scan(&data, &roleData, &active); err != nil {
		return agent, err
	}
	if err := json.Unmarshal(data, &agent); err != nil {
		return agent, err
	}
	if err := json.Unmarshal(roleData, &agent.Role); err != nil {
		return agent, err
	}
	// Cost tier was added after the first team members. Keep historical records
	// readable and make the existing Luna low-cost members immediately usable.
	agent.CostTier = model.NormalizeCostTier(agent.CostTier, agent.ModelID)
	agent.ActiveRuns = active
	return agent, nil
}

func (s *Store) CreateAgent(ctx context.Context, agent model.AgentProfile) (model.AgentProfile, error) {
	agent.Name = strings.TrimSpace(agent.Name)
	if agent.Name == "" || len(agent.Name) > 200 || agent.RoleID == "" || agent.RuntimeID == "" || agent.AdapterID == "" {
		return agent, fmt.Errorf("%w: name, role_id, runtime_id and adapter_id are required", model.ErrValidation)
	}
	if agent.MaxConcurrent == 0 {
		agent.MaxConcurrent = 1
	}
	if agent.MaxConcurrent < 1 || agent.MaxConcurrent > 32 {
		return agent, fmt.Errorf("%w: max_concurrent must be between 1 and 32", model.ErrValidation)
	}
	if len(agent.ModelID) > 200 {
		return agent, fmt.Errorf("%w: model_id too long", model.ErrValidation)
	}
	requestedTier := strings.ToLower(strings.TrimSpace(agent.CostTier))
	if requestedTier != "" && requestedTier != model.CostTierEconomy && requestedTier != model.CostTierStandard && requestedTier != model.CostTierPremium {
		return agent, fmt.Errorf("%w: invalid cost_tier", model.ErrValidation)
	}
	agent.CostTier = model.NormalizeCostTier(requestedTier, agent.ModelID)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return agent, err
	}
	defer tx.Rollback()
	role, err := readJSONRow[model.Role](tx.QueryRowContext(ctx, `SELECT data_json FROM role WHERE role_id = ?`, agent.RoleID))
	if err != nil {
		return agent, err
	}
	if err := validateRuntimeEffortTx(ctx, tx, agent.RuntimeID, agent.AdapterID, agent.ReasoningEffort); err != nil {
		return agent, err
	}
	var capabilityJSON []byte
	if err := tx.QueryRowContext(ctx, `SELECT capabilities_json FROM runtime WHERE runtime_id = ?`, agent.RuntimeID).Scan(&capabilityJSON); err != nil {
		return agent, err
	}
	if !router.SupportsFeature(model.Runtime{Capabilities: capabilityJSON}, agent.AdapterID, "role_instructions") {
		return agent, fmt.Errorf("%w: adapter does not advertise role_instructions support", model.ErrValidation)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_profile WHERE name = ?`, agent.Name).Scan(&exists); err != nil {
		return agent, err
	}
	if exists > 0 {
		return agent, fmt.Errorf("%w: agent name already exists", model.ErrConflict)
	}
	agent.ID, agent.State, agent.CreatedAtMS, agent.Role, agent.ActiveRuns = id.New("agent"), "ACTIVE", time.Now().UTC().UnixMilli(), role, 0
	data, _ := json.Marshal(agent)
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_profile(agent_id,name,role_id,runtime_id,data_json) VALUES(?,?,?,?,?)`, agent.ID, agent.Name, agent.RoleID, agent.RuntimeID, data); err != nil {
		return agent, err
	}
	if _, err := appendEventTx(ctx, tx, "agent", agent.ID, "AgentCreated", "", agent.ID, agent); err != nil {
		return agent, err
	}
	return agent, tx.Commit()
}

func validateRequirementsTx(ctx context.Context, tx *sql.Tx, needs model.TaskRequirements) error {
	if err := needs.Validate(); err != nil {
		return err
	}
	if needs.RoleID != "" {
		var roleID string
		if err := tx.QueryRowContext(ctx, `SELECT role_id FROM role WHERE role_id = ?`, needs.RoleID).Scan(&roleID); err != nil {
			return err
		}
	}
	return nil
}

// Revalidate inside the writer transaction, so concurrent dispatches cannot
// exceed capacity or bypass role/exclusion constraints through explicit IDs.
func validateAgentAssignmentTx(ctx context.Context, tx *sql.Tx, session model.Session, taskID string) (*model.Role, error) {
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	agent, err := scanAgent(tx.QueryRowContext(ctx, agentSelect+` WHERE a.agent_id = ?`, session.AgentID))
	if err == sql.ErrNoRows && task.Requirements.IsEmpty() {
		return nil, nil
	}
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: task requires a registered role agent", model.ErrConflict)
	}
	if err != nil {
		return nil, err
	}
	// Existing sessions use their pinned role and model even after edits. New
	// sessions have no snapshot yet and must match the current configuration.
	var metadata struct {
		Role *model.Role `json:"role_snapshot"`
	}
	if err := json.Unmarshal(session.Metadata, &metadata); err != nil {
		return nil, err
	}
	bound := metadata.Role != nil
	if bound {
		agent.Role = *metadata.Role
		agent.RoleID = metadata.Role.ID
	}
	if !router.Matches(task.Requirements, agent) {
		return nil, fmt.Errorf("%w: agent does not satisfy task role, capabilities or exclusion constraints", model.ErrConflict)
	}
	if agent.RuntimeID != session.RuntimeID || agent.AdapterID != session.AdapterID || (!bound && agent.ModelID != session.ModelID) {
		return nil, fmt.Errorf("%w: agent configuration conflicts with session", model.ErrConflict)
	}
	if agent.State != "ACTIVE" || agent.ActiveRuns >= agent.MaxConcurrent {
		return nil, fmt.Errorf("%w: agent has no available capacity", model.ErrConflict)
	}
	return &agent.Role, nil
}
