package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"work-assistant/internal/model"
)

// Updating a role appends a revision. Existing Runs and Sessions retain their
// snapshots; the stable role identity is used for routing new tasks.
func (s *Store) UpdateRole(ctx context.Context, roleID string, expected int64, spec model.RoleSpec) (model.Role, error) {
	if err := spec.Validate(true); err != nil {
		return model.Role{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Role{}, err
	}
	defer tx.Rollback()
	role, err := readJSONRow[model.Role](tx.QueryRowContext(ctx, `SELECT data_json FROM role WHERE role_id=?`, roleID))
	if err != nil {
		return role, err
	}
	if role.Version != expected {
		return role, fmt.Errorf("%w: role changed; reload before editing", model.ErrConflict)
	}
	previous, _ := json.Marshal(role)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO role_revision VALUES(?,?,?)`, roleID, role.Version, previous); err != nil {
		return role, err
	}
	role.RoleSpec = spec
	role.Version++
	data, _ := json.Marshal(role)
	if _, err = tx.ExecContext(ctx, `INSERT INTO role_revision VALUES(?,?,?)`, roleID, role.Version, data); err != nil {
		return role, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE role SET data_json=? WHERE role_id=?`, data, roleID); err != nil {
		return role, err
	}
	if _, err = appendEventTx(ctx, tx, "role", roleID, "RoleRevisionPublished", "", roleID, role); err != nil {
		return role, err
	}
	return role, tx.Commit()
}

type AgentUpdate struct {
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	Name            string  `json:"name"`
	ModelID         string  `json:"model_id"`
	MaxConcurrent   int     `json:"max_concurrent"`
	State           string  `json:"state"`
	ExpectedVersion int64   `json:"expected_version"`
}

// Machine and adapter identity are stable. Model changes affect new sessions
// only. Disabling prevents dispatch but never silently kills active work.
func (s *Store) UpdateAgent(ctx context.Context, agentID string, u AgentUpdate) (model.AgentProfile, error) {
	if strings.TrimSpace(u.Name) == "" || len(u.Name) > 200 || len(u.ModelID) > 200 || u.MaxConcurrent < 1 || u.MaxConcurrent > 32 || (u.State != "ACTIVE" && u.State != "DISABLED") {
		return model.AgentProfile{}, fmt.Errorf("%w: invalid agent configuration", model.ErrValidation)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AgentProfile{}, err
	}
	defer tx.Rollback()
	a, err := scanAgent(tx.QueryRowContext(ctx, agentSelect+` WHERE a.agent_id=?`, agentID))
	if err != nil {
		return a, err
	}
	if a.Version != u.ExpectedVersion {
		return a, fmt.Errorf("%w: agent changed; reload before editing", model.ErrConflict)
	}
	effort := a.ReasoningEffort
	if u.ReasoningEffort != nil {
		effort = *u.ReasoningEffort
	} else if a.ModelID != u.ModelID {
		effort = ""
	}
	if err := validateRuntimeEffortTx(ctx, tx, a.RuntimeID, a.AdapterID, effort); err != nil {
		return a, err
	}
	if u.MaxConcurrent < a.ActiveRuns {
		return a, fmt.Errorf("%w: concurrency cannot be reduced below active runs", model.ErrConflict)
	}
	var duplicates int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_profile WHERE name=? AND agent_id!=?`, strings.TrimSpace(u.Name), agentID).Scan(&duplicates); err != nil {
		return a, err
	}
	if duplicates > 0 {
		return a, fmt.Errorf("%w: agent name already exists", model.ErrConflict)
	}
	a.Name, a.ModelID, a.MaxConcurrent, a.State = strings.TrimSpace(u.Name), u.ModelID, u.MaxConcurrent, u.State
	a.ReasoningEffort = effort
	a.Version++
	data, _ := json.Marshal(a)
	if _, err = tx.ExecContext(ctx, `UPDATE agent_profile SET name=?,data_json=? WHERE agent_id=?`, a.Name, data, agentID); err != nil {
		return a, err
	}
	if _, err = appendEventTx(ctx, tx, "agent", agentID, "AgentUpdated", "", agentID, a); err != nil {
		return a, err
	}
	return a, tx.Commit()
}
