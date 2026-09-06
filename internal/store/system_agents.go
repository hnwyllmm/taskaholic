package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/router"
)

func migrateV9(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version=9`).Scan(&n); err != nil || n > 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE system_binding(slot TEXT PRIMARY KEY,data_json TEXT NOT NULL)`,
		`CREATE TABLE routing_decision(decision_id TEXT PRIMARY KEY,task_id TEXT NOT NULL REFERENCES task(task_id),task_version INTEGER NOT NULL,internal_task_id TEXT NOT NULL UNIQUE REFERENCES task(task_id),data_json TEXT NOT NULL,UNIQUE(task_id,task_version))`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	for _, slot := range model.SystemSlots {
		b := model.SystemBinding{Slot: slot.ID, Mode: slot.DefaultMode, Version: 1, UpdatedAtMS: time.Now().UnixMilli()}
		raw, _ := json.Marshal(b)
		if _, err = tx.Exec(`INSERT INTO system_binding VALUES(?,?)`, b.Slot, raw); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO schema_version VALUES(9,unixepoch('subsec')*1000)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListSystemBindings(ctx context.Context) ([]model.SystemBinding, error) {
	return listJSONRows[model.SystemBinding](ctx, s.db, `SELECT data_json FROM system_binding ORDER BY slot`)
}
func (s *Store) GetSystemBinding(ctx context.Context, slot string) (model.SystemBinding, error) {
	return readJSONRow[model.SystemBinding](s.db.QueryRowContext(ctx, `SELECT data_json FROM system_binding WHERE slot=?`, slot))
}
func getSystemBindingTx(ctx context.Context, tx *sql.Tx, slot string) (model.SystemBinding, error) {
	return readJSONRow[model.SystemBinding](tx.QueryRowContext(ctx, `SELECT data_json FROM system_binding WHERE slot=?`, slot))
}

// LocalRuntimeID is supplied by the trusted server, never by the request body.
func (s *Store) UpdateSystemBinding(ctx context.Context, b model.SystemBinding, expected int64, localRuntimeID string) (model.SystemBinding, error) {
	slot, ok := model.FindSystemSlot(b.Slot)
	if !ok || (b.Mode != slot.DefaultMode && b.Mode != "agent") || (b.Mode == "agent") != (b.AgentID != "") {
		return b, fmt.Errorf("%w: invalid system slot, mode or member", model.ErrValidation)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return b, err
	}
	defer tx.Rollback()
	current, err := getSystemBindingTx(ctx, tx, b.Slot)
	if err != nil {
		return b, err
	}
	if current.Version != expected {
		return b, fmt.Errorf("%w: 系统岗位已变更，请刷新后保存", model.ErrConflict)
	}
	if b.Mode == "agent" {
		a, e := systemAgentTx(ctx, tx, b.AgentID, b.Slot)
		if e != nil {
			return b, e
		}
		if b.Slot == "upgrade_builder" && (localRuntimeID == "" || a.RuntimeID != localRuntimeID || (a.AdapterID != "codex-agent" && a.AdapterID != "cursor-agent")) {
			return b, fmt.Errorf("%w: 升级构建支持升级守护进程所在机器的 Codex 或 Cursor 成员", model.ErrValidation)
		}
	}
	b.Version = current.Version + 1
	b.UpdatedAtMS = time.Now().UnixMilli()
	raw, _ := json.Marshal(b)
	if _, err = tx.ExecContext(ctx, `UPDATE system_binding SET data_json=? WHERE slot=?`, raw, b.Slot); err != nil {
		return b, err
	}
	if _, err = appendEventTx(ctx, tx, "system_binding", b.Slot, "SystemAgentChanged", "", b.Slot, map[string]any{"before": current, "after": b}); err != nil {
		return b, err
	}
	return b, tx.Commit()
}

func systemAgentTx(ctx context.Context, tx *sql.Tx, agentID, slot string) (model.AgentProfile, error) {
	a, err := scanAgent(tx.QueryRowContext(ctx, agentSelect+` WHERE a.agent_id=?`, agentID))
	if err == sql.ErrNoRows {
		return a, fmt.Errorf("%w: 系统岗位成员不存在", model.ErrValidation)
	}
	if err != nil {
		return a, err
	}
	if a.State != "ACTIVE" {
		return a, fmt.Errorf("%w: 系统岗位成员已停用", model.ErrConflict)
	}
	var caps []byte
	if err = tx.QueryRowContext(ctx, `SELECT capabilities_json FROM runtime WHERE runtime_id=?`, a.RuntimeID).Scan(&caps); err != nil {
		return a, err
	}
	features := []string{"role_instructions", "structured_output"}
	if slot != "upgrade_builder" {
		features = append(features, "read_only_runs")
	}
	for _, f := range features {
		if !router.SupportsFeature(model.Runtime{Capabilities: caps}, a.AdapterID, f) {
			return a, fmt.Errorf("%w: 成员适配器缺少 %s 能力", model.ErrValidation, f)
		}
	}
	return a, nil
}

func checkSystemBindingTx(ctx context.Context, tx *sql.Tx, req CreateRunRequest) error {
	if req.SystemBinding == nil {
		return nil
	}
	b, err := getSystemBindingTx(ctx, tx, req.SystemBinding.Slot)
	if err != nil {
		return err
	}
	if b.Version != req.SystemBinding.Version || b.Mode != req.SystemBinding.Mode || b.AgentID != req.SystemBinding.AgentID {
		return fmt.Errorf("%w: 系统岗位已变更，请重试", model.ErrConflict)
	}
	if b.Mode == "agent" && b.AgentID != req.AgentID {
		return fmt.Errorf("%w: system executor differs from configured member", model.ErrConflict)
	}
	return nil
}
