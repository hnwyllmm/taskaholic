package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"work-assistant/internal/model"
)

// Only an explicit human request may rebind a homepage conversation. Business
// tasks have no corresponding endpoint. Old sessions and records are retained.
func (s *Store) SwitchHomeExecutor(ctx context.Context, chatID string, expected int64, req CreateRunRequest) (model.HomeChat, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.HomeChat{}, err
	}
	defer tx.Rollback()
	c, err := getHomeChatTx(ctx, tx, chatID)
	if err != nil {
		return c, err
	}
	if c.Version != expected || c.State == "GENERATING" {
		return c, fmt.Errorf("%w: 对话已变化或仍在运行，请先停止并等待确认", model.ErrConflict)
	}
	if len(c.Messages) >= 100 {
		return c, fmt.Errorf("%w: 此对话记录已达到上限，请新建对话；旧记录保留", model.ErrConflict)
	}
	if err = checkMaintenanceTx(ctx, tx); err != nil {
		return c, err
	}
	if req.SystemBinding == nil || req.SystemBinding.Slot != "home_chat" {
		return c, fmt.Errorf("%w: missing home slot confirmation", model.ErrValidation)
	}
	if err = checkSystemBindingTx(ctx, tx, req); err != nil {
		return c, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')`, c.TaskID).Scan(&active); err != nil {
		return c, err
	}
	if active != 0 {
		return c, fmt.Errorf("%w: 对话运行尚未结束", model.ErrConflict)
	}
	old, err := getActiveSessionTx(ctx, tx, c.TaskID)
	if err != nil && err != sql.ErrNoRows {
		return c, err
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `UPDATE task_session SET unbound_at_ms=? WHERE task_id=? AND unbound_at_ms IS NULL`, now, c.TaskID); err != nil {
		return c, err
	}
	req.TaskID = c.TaskID
	req.SessionID = ""
	session, _, _, err := ensureTaskSessionTx(ctx, tx, req, now)
	if err != nil {
		return c, err
	}
	role, err := validateAgentAssignmentTx(ctx, tx, session, c.TaskID)
	if err != nil {
		return c, err
	}
	execution, err := resolveExecutionTx(ctx, tx, session, req.ReasoningEffort)
	if err != nil {
		return c, err
	}
	session.Metadata, _ = json.Marshal(map[string]any{"role_snapshot": role, "system_binding": req.SystemBinding, "handoff_from": old.ID, "execution_defaults": execution})
	if _, err = tx.ExecContext(ctx, `UPDATE session SET metadata_json=? WHERE session_id=?`, session.Metadata, session.ID); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task SET assigned_agent_id=?,version=version+1,updated_at_ms=? WHERE task_id=?`, session.AgentID, now, c.TaskID); err != nil {
		return c, err
	}
	for i := range c.Proposals {
		if c.Proposals[i].State == "PENDING" {
			c.Proposals[i].State = "SUPERSEDED"
		}
	}
	c.State = "IDLE"
	c.Error = ""
	c.Messages = append(c.Messages, model.RoleMessage{Speaker: "system", Content: "你已确认更换聊天执行者。旧 Session 与记录保留；新成员将在独立 Session 中收到最近可见对话，未确认的旧建议已失效。", CreatedAtMS: now})
	if err = saveHomeTx(ctx, tx, &c, "HomeExecutorChanged"); err != nil {
		return c, err
	}
	if _, err = appendEventTx(ctx, tx, "session", session.ID, "SessionHandoffConfirmed", "", c.TaskID, map[string]any{"old_session_id": old.ID, "new_session_id": session.ID, "agent_id": session.AgentID, "model_id": session.ModelID, "binding": req.SystemBinding}); err != nil {
		return c, err
	}
	if err = tx.Commit(); err != nil {
		return c, err
	}
	c.Session = &session
	return c, nil
}
