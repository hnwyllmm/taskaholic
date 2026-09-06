package store

import (
	"context"
	"database/sql"
	"fmt"

	"work-assistant/internal/model"
	"work-assistant/internal/router"
)

// Validate role/capability constraints at the assignment boundary. Offline or
// busy members are intentionally allowed: naming a member means waiting for
// that member, not quietly choosing somebody else.
func validateWorkMemberTx(ctx context.Context, tx *sql.Tx, task model.Task, agentID string) (model.AgentProfile, error) {
	a, err := scanAgent(tx.QueryRowContext(ctx, agentSelect+` WHERE a.agent_id=?`, agentID))
	if err != nil {
		return a, err
	}
	if a.State != "ACTIVE" {
		return a, fmt.Errorf("%w: 所选成员已停用，请先启用或选择其他成员", model.ErrValidation)
	}
	if !router.Matches(task.Requirements, a) {
		return a, fmt.Errorf("%w: 所选成员不满足任务的角色、能力或排除约束", model.ErrValidation)
	}
	return a, nil
}

// AssignWork changes scheduling intent only before the first business Session.
// The version guard and first-Run check share a transaction with the scheduler;
// a concurrent start or another user's change must win or lose, never mix.
func (s *Store) AssignWork(ctx context.Context, taskID, agentID string, expectedVersion int64) (model.Task, error) {
	if expectedVersion < 1 || len(agentID) > 200 {
		return model.Task{}, fmt.Errorf("%w: expected_version is required; agent_id maximum 200 bytes", model.ErrValidation)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Task{}, err
	}
	defer tx.Rollback()
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return task, err
	}
	w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, taskID))
	if err != nil {
		return task, err
	}
	if task.Version != expectedVersion {
		return task, fmt.Errorf("%w: 任务状态或分派已变化，请刷新后重新确认", model.ErrConflict)
	}
	switch task.State {
	case model.TaskStateNew, model.TaskStateQueued, model.TaskStatePaused, model.TaskStateBlocked:
	default:
		return task, fmt.Errorf("%w: 当前任务不能重新分派", model.ErrConflict)
	}
	var started bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE task_id=?) OR EXISTS(SELECT 1 FROM task_session WHERE task_id=?)`, taskID, taskID).Scan(&started); err != nil {
		return task, err
	}
	if started || task.AssignedAgentID != "" {
		return task, fmt.Errorf("%w: 任务已绑定原成员和 Session，不能直接换人或重新自动分派", model.ErrConflict)
	}
	mode, message := "auto", "已请求自动分派；系统将根据任务要求、成员能力、在线状态和空闲容量选择执行者。"
	if agentID != "" {
		a, err := validateWorkMemberTx(ctx, tx, task, agentID)
		if err != nil {
			return task, err
		}
		mode, message = "member", "已指定「"+a.Name+"」执行；若成员忙碌或离线，将等待该成员，不自动换人。"
	}
	if err = supersedeWorkRoutingTx(ctx, tx, taskID); err != nil {
		return task, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET agent_id=?,paused=0,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, agentID, taskID); err != nil {
		return task, err
	}
	if _, err = insertMessageTx(ctx, tx, taskID, "system", message, "", "RECORDED"); err != nil {
		return task, err
	}
	if err = setWorkStateTx(ctx, tx, taskID, model.TaskStateQueued); err != nil {
		return task, err
	}
	if _, err = appendEventTx(ctx, tx, "task", taskID, "TaskAssignmentRequested", "", taskID, map[string]any{"mode": mode, "agent_id": agentID, "previous_agent_id": w.AgentID, "expected_version": expectedVersion}); err != nil {
		return task, err
	}
	task, err = getTaskTx(ctx, tx, taskID)
	if err != nil {
		return task, err
	}
	return task, tx.Commit()
}

// A superseded AI router may not override the user's newer intent. Keep its
// history and issue a durable interrupt for its internal Run, if still active.
func supersedeWorkRoutingTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT data_json FROM routing_decision WHERE task_id=? AND json_extract(data_json,'$.state') IN ('GENERATING','READY')`, taskID)
	if err != nil {
		return err
	}
	var decisions []model.RoutingDecision
	for rows.Next() {
		d, err := readJSONRow[model.RoutingDecision](rows)
		if err != nil {
			rows.Close()
			return err
		}
		decisions = append(decisions, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range decisions {
		var active bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE run_id=? AND state IN ('QUEUED','RUNNING'))`, d.RunID).Scan(&active); err != nil {
			return err
		}
		if active {
			if err = interruptWorkTx(ctx, tx, d.InternalTaskID, d.RunID); err != nil {
				return err
			}
		}
		d.State, d.Error = "SUPERSEDED", "任务分派已由用户更新"
		if err = saveRoutingTx(ctx, tx, d, "TaskRoutingSuperseded"); err != nil {
			return err
		}
	}
	return nil
}
