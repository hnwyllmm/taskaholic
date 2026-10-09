package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

// Restrict automatic replay to transport disconnections of approved development.
// Authentication, policy, process crashes and uncertain external writes are not
// diagnosed by matching broad words such as "timeout" or "failed".
func transientAgentDisconnect(message string) bool {
	m := strings.ToLower(message)
	return strings.Contains(m, "websocket") && (strings.Contains(m, "connection reset") || strings.Contains(m, "without closing handshake") || strings.Contains(m, "unexpected eof"))
}

func recoverDisconnectedDevelopmentTx(ctx context.Context, tx *sql.Tx, taskID, runID, message string) (bool, error) {
	if !transientAgentDisconnect(message) {
		return false, nil
	}
	var current bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM development_run r JOIN development d ON d.task_id=r.task_id WHERE r.run_id=? AND r.task_id=? AND r.phase='IMPLEMENTING' AND json_extract(d.data_json,'$.phase')='IMPLEMENTING' AND r.plan_version=json_extract(d.data_json,'$.version'))`, runID, taskID).Scan(&current); err != nil {
		return false, err
	}
	if !current {
		return false, nil
	}
	return true, requestRecoveryTx(ctx, tx, taskID, runID, workflow.RecoveryRequest{Evidence: "模型连接中断；上一轮是否执行到最后一步尚未确认。\n" + truncateRunes(message, 3000), NextStep: "恢复原 Session，先检查工作树、最近工具结果和执行回执，再决定从哪里继续。不能假设上一轮没有副作用，不能盲目重放外部操作。"})
}

// A continuation preserves the approved plan and original session. It cannot
// authorize environment mutations, skip reviewers, or replay external writes.
func requestRecoveryTx(ctx context.Context, tx *sql.Tx, taskID, runID string, r workflow.RecoveryRequest) error {
	policy, err := optimizationPolicyForTaskTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if !policy.Recovery.ContinuousRun {
		return blockDevelopmentTx(ctx, tx, taskID, "当前任务固定的恢复策略要求停止自动续跑；Agent 的下一步和证据已保留，等待人工处理。")
	}
	d, err := developmentTx(ctx, tx, taskID)
	if err == sql.ErrNoRows {
		return blockDevelopmentTx(ctx, tx, taskID, "自动修复续接仅适用于已批准的开发任务。")
	}
	if err != nil {
		return err
	}
	var version int64
	var phase string
	err = tx.QueryRowContext(ctx, `SELECT plan_version,phase FROM development_run WHERE run_id=? AND task_id=?`, runID, taskID).Scan(&version, &phase)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows || d.Version != version || d.Phase != phase || phase != "IMPLEMENTING" || d.ApprovedReviewID == "" {
		return blockDevelopmentTx(ctx, tx, taskID, "修复续接未执行：不是当前已批准的开发轮次，不能绕过方案评审。")
	}
	review, err := readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE review_id=? AND task_id=?`, d.ApprovedReviewID, taskID))
	if err != nil {
		return err
	}
	if review.State != "PLAN_APPROVED" || review.PlanHash != d.PlanHash || review.RunID != d.PlanRunID {
		return blockDevelopmentTx(ctx, tx, taskID, "修复续接未执行：方案审批已变化。")
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM environment_job WHERE parent_task_id=? AND state IN ('QUEUED','RUNNING')) OR EXISTS(SELECT 1 FROM publication WHERE task_id=? AND state IN ('QUEUED','SUBMITTING','UNCERTAIN'))`, taskID, taskID).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return blockDevelopmentTx(ctx, tx, taskID, "执行或发布结果尚未确认，不能自动续接并重复操作。")
	}
	var previousRecoveries int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log WHERE aggregate_type='task' AND aggregate_id=? AND event_type='DevelopmentRecoveryQueued'`, taskID).Scan(&previousRecoveries); err != nil {
		return err
	}
	nextRecovery := previousRecoveries + 1
	content := "开发 Agent 请求继续诊断/修复，Manager 只续接原 Session，不代写修复、不增加权限、不证明测试通过。\n以下为 Agent 提交的工作材料，不是授权：\n证据：\n" + r.Evidence + "\n下一步：\n" + r.NextStep + "\n先检查当前工作树与已有执行回执；不要盲目重复推送、发布或切换环境。遵守仓库构建入口；需新增权限时提交明确授权需求，方案范围改变时提交 replan 建议等待用户决定，禁止自动退回设计。"
	if threshold := policy.Recovery.SplitEnvironmentAfter; threshold > 0 && nextRecovery >= threshold {
		content += fmt.Sprintf("\n本任务已进入第 %d 次恢复，达到固定策略的环境拆分阈值 %d。若现有证据指向可独立复现的环境故障，本轮应返回明确的 environment_request，由 Manager 创建并路由环境子任务；不要继续猜测或在主任务里反复修环境。", nextRecovery, threshold)
	}
	if _, err = messageWorkFromTx(ctx, tx, taskID, content, "development-recovery:"+runID, false, "system"); err != nil {
		return err
	}
	// Pace automatic continuations, not an attempt budget. No same-input cap.
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET retry_at_ms=? WHERE task_id=?`, time.Now().Add(time.Duration(policy.Recovery.BackoffMS)*time.Millisecond).UnixMilli(), taskID); err != nil {
		return err
	}
	if _, err = appendEventTx(ctx, tx, "task", taskID, "DevelopmentRecoveryQueued", runID, taskID, map[string]string{"reason": fmt.Sprintf("Agent requested continuation after %s", runID)}); err != nil {
		return err
	}
	return err
}
