package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
	"work-assistant/internal/router"
	"work-assistant/internal/workflow"
)

func migrateV17(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil || v >= 17 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE development(task_id TEXT PRIMARY KEY REFERENCES task(task_id), data_json TEXT NOT NULL)`,
		`CREATE TABLE development_run(run_id TEXT PRIMARY KEY REFERENCES run(run_id), task_id TEXT NOT NULL REFERENCES development(task_id), plan_version INTEGER NOT NULL, phase TEXT NOT NULL)`,
		`INSERT INTO schema_version VALUES(17,unixepoch('subsec')*1000)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return fmt.Errorf("migrate v17: %w", err)
		}
	}
	return tx.Commit()
}

func developmentTx(ctx context.Context, tx *sql.Tx, taskID string) (model.Development, error) {
	return readJSONRow[model.Development](tx.QueryRowContext(ctx, `SELECT data_json FROM development WHERE task_id=?`, taskID))
}
func (s *Store) GetDevelopment(ctx context.Context, taskID string) (model.Development, error) {
	return readJSONRow[model.Development](s.db.QueryRowContext(ctx, `SELECT data_json FROM development WHERE task_id=?`, taskID))
}

// Retry a failed runtime attempt, not a change of requirements or new approval.
func (s *Store) RetryDevelopment(ctx context.Context, taskID string, expected int64) (model.TaskMessage, error) {
	var message model.TaskMessage
	if m, err := s.Maintenance(ctx); err != nil {
		return message, err
	} else if m != "" {
		return message, fmt.Errorf("%w: maintenance active", model.ErrConflict)
	}
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		t, err := getTaskTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if t.Version != expected || t.State != model.TaskStateBlocked {
			return fmt.Errorf("%w: retry requires the current blocked task version", model.ErrConflict)
		}
		d, err := developmentTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if d.Phase != "IMPLEMENTING" || d.ApprovedReviewID == "" {
			return fmt.Errorf("%w: retry cannot replace plan approval", model.ErrConflict)
		}
		r, err := readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE review_id=? AND task_id=?`, d.ApprovedReviewID, taskID))
		if err != nil {
			return err
		}
		if r.State != "PLAN_APPROVED" || r.PlanHash != d.PlanHash || r.RunID != d.PlanRunID {
			return fmt.Errorf("%w: approved plan changed", model.ErrConflict)
		}
		var runID, state string
		if err = tx.QueryRowContext(ctx, `SELECT run_id,state FROM run WHERE task_id=? ORDER BY created_at_ms DESC LIMIT 1`, taskID).Scan(&runID, &state); err != nil {
			return err
		}
		if state != "FAILED" {
			return fmt.Errorf("%w: only failed runtime attempts can be retried", model.ErrConflict)
		}
		var busy bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING') OR EXISTS(SELECT 1 FROM review_turn WHERE task_id=? AND state IN ('QUEUED','RUNNING'))`, taskID, taskID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return fmt.Errorf("%w: task has pending work", model.ErrConflict)
		}
		message, err = messageWorkFromTx(ctx, tx, taskID, "运行环境已修复，请沿用当前已批准方案、原 Agent 和原 Session 重试开发。需求及审批范围不变。", "development-retry:"+runID, false, "system")
		return err
	})
	return message, err
}

// ApprovedValidationPlanAmendment is a user-authorized change to the evidence
// required to ship an already approved implementation. It deliberately has no
// repository, branch, plan or execution-grant fields: those are immutable here.
// This is for cases such as an unavailable platform runner, not for changing
// product behavior or implementation scope.
type ApprovedValidationPlanAmendment struct {
	ExpectedVersion int64
	Reason          string
	ValidationPlan  model.DevelopmentValidationPlan
	IdempotencyKey  string
}

// AmendApprovedValidationPlan replaces only the validation gate of an idle,
// human-approved implementation and resumes its existing Session. A human must
// make this explicit through the authenticated API; an Agent result cannot use
// this path to weaken its own approval requirements.
func (s *Store) AmendApprovedValidationPlan(ctx context.Context, taskID string, req ApprovedValidationPlanAmendment) (model.Development, error) {
	var amended model.Development
	req.Reason = strings.TrimSpace(req.Reason)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.Reason == "" || len(req.Reason) > 4000 {
		return amended, fmt.Errorf("%w: validation scope amendment reason required", model.ErrValidation)
	}
	if len(req.IdempotencyKey) > 256 {
		return amended, fmt.Errorf("%w: idempotency key is too long", model.ErrValidation)
	}
	plan, err := normalizeDevelopmentValidationPlan(req.ValidationPlan)
	if err != nil {
		return amended, err
	}
	if maintenance, err := s.Maintenance(ctx); err != nil {
		return amended, err
	} else if maintenance != "" {
		return amended, fmt.Errorf("%w: maintenance active", model.ErrConflict)
	}
	err = s.sourceWrite(ctx, func(tx *sql.Tx) error {
		const scopePrefix = "development.validation-amendment:"
		if req.IdempotencyKey != "" {
			var existing string
			err := tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope=? AND key=?`, scopePrefix+taskID, req.IdempotencyKey).Scan(&existing)
			if err == nil {
				amended, err = developmentTx(ctx, tx, taskID)
				return err
			}
			if err != sql.ErrNoRows {
				return err
			}
		}

		task, err := getTaskTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if task.Version != req.ExpectedVersion || (task.State != model.TaskStatePaused && task.State != model.TaskStateBlocked) {
			return fmt.Errorf("%w: validation scope amendment requires the current paused or blocked task version", model.ErrConflict)
		}
		d, err := developmentTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if d.Phase != "IMPLEMENTING" || d.ApprovedReviewID == "" || d.PlanHash == "" || d.PlanRunID == "" || d.ValidationPlan == nil || model.ValidateDevelopmentRepository(d.Repository, d.BaseBranch) != nil {
			return fmt.Errorf("%w: validation scope amendment requires an approved implementation", model.ErrConflict)
		}
		review, err := readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE review_id=? AND task_id=?`, d.ApprovedReviewID, taskID))
		if err != nil {
			return err
		}
		if review.State != "PLAN_APPROVED" || review.PlanHash != d.PlanHash || review.RunID != d.PlanRunID {
			return fmt.Errorf("%w: approved plan changed", model.ErrConflict)
		}
		var busy bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING'
			UNION ALL SELECT 1 FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')
			UNION ALL SELECT 1 FROM review WHERE task_id=? AND state='PENDING'
			UNION ALL SELECT 1 FROM review_turn WHERE task_id=? AND state IN ('QUEUED','RUNNING')
			UNION ALL SELECT 1 FROM environment_job WHERE parent_task_id=? AND state IN ('QUEUED','RUNNING')
			UNION ALL SELECT 1 FROM publication WHERE task_id=? AND state IN ('QUEUED','SUBMITTING','UNCERTAIN')
		)`, taskID, taskID, taskID, taskID, taskID, taskID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return fmt.Errorf("%w: task still has active work, review, environment or publication", model.ErrConflict)
		}

		previous := d.ValidationPlan
		d.ValidationPlan = &plan
		if err = saveDevelopmentTx(ctx, tx, d, "UserValidationScopeAmended"); err != nil {
			return err
		}
		message, err := messageWorkFromTx(ctx, tx, taskID, "用户已受控确认调整本任务的验证范围（不是重新设计）：\n"+req.Reason+"\n\n已保留原人工批准的代码方案、仓库/分支、plan hash、执行授权和 PR 目标；不要重新提交方案、不要邀请方案 reviewer。请沿用原 Session 和现有隔离工作树，严格执行当前结构化 validation_plan。未覆盖的平台或构建门禁必须在交付/PR 验证说明中如实列为用户接受的范围限制，不能标为通过；完成新的 final_gate 后直接提交 publish_request。", "", false, "system")
		if err != nil {
			return err
		}
		if _, err = appendEventTx(ctx, tx, "task", taskID, "UserValidationScopeAmendmentAccepted", message.ID, taskID, map[string]any{
			"reason":                   req.Reason,
			"message_id":               message.ID,
			"plan_hash":                d.PlanHash,
			"approved_review_id":       d.ApprovedReviewID,
			"previous_validation_plan": previous,
			"validation_plan":          d.ValidationPlan,
		}); err != nil {
			return err
		}
		if req.IdempotencyKey != "" {
			if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_key(scope,key,resource_id,created_at_ms) VALUES(?,?,?,?)`, scopePrefix+taskID, req.IdempotencyKey, message.ID, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		amended = d
		return nil
	})
	return amended, err
}

func normalizeDevelopmentValidationPlan(plan model.DevelopmentValidationPlan) (model.DevelopmentValidationPlan, error) {
	normalized := model.DevelopmentValidationPlan{}
	normalizeItems := func(items []model.DevelopmentValidationItem) ([]model.DevelopmentValidationItem, error) {
		out := make([]model.DevelopmentValidationItem, 0, len(items))
		for _, item := range items {
			item.Scenario = strings.TrimSpace(item.Scenario)
			item.Reason = strings.TrimSpace(item.Reason)
			item.Evidence = strings.TrimSpace(item.Evidence)
			if item.Scenario == "" || item.Reason == "" || len(item.Scenario) > 500 || len(item.Reason) > 2000 || len(item.Evidence) > 2000 {
				return nil, fmt.Errorf("%w: invalid validation plan item", model.ErrValidation)
			}
			out = append(out, item)
		}
		return out, nil
	}
	var err error
	if normalized.Reuse, err = normalizeItems(plan.Reuse); err != nil {
		return normalized, err
	}
	if normalized.Rerun, err = normalizeItems(plan.Rerun); err != nil {
		return normalized, err
	}
	if normalized.Add, err = normalizeItems(plan.Add); err != nil {
		return normalized, err
	}
	if normalized.Exclude, err = normalizeItems(plan.Exclude); err != nil {
		return normalized, err
	}
	count := len(normalized.Reuse) + len(normalized.Rerun) + len(normalized.Add) + len(normalized.Exclude)
	if count == 0 || count > 128 {
		return normalized, fmt.Errorf("%w: validation plan requires classified scenarios", model.ErrValidation)
	}
	if len(plan.FinalGate) == 0 || len(plan.FinalGate) > 32 {
		return normalized, fmt.Errorf("%w: validation plan requires 1 to 32 final gates", model.ErrValidation)
	}
	normalized.FinalGate = make([]string, 0, len(plan.FinalGate))
	for _, gate := range plan.FinalGate {
		gate = strings.TrimSpace(gate)
		if gate == "" || len(gate) > 1000 {
			return model.DevelopmentValidationPlan{}, fmt.Errorf("%w: invalid validation final gate", model.ErrValidation)
		}
		normalized.FinalGate = append(normalized.FinalGate, gate)
	}
	return normalized, nil
}

func saveDevelopmentTx(ctx context.Context, tx *sql.Tx, d model.Development, event string) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO development VALUES(?,?) ON CONFLICT(task_id) DO UPDATE SET data_json=excluded.data_json`, d.TaskID, raw); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "task", d.TaskID, event, "", d.TaskID, d)
	return err
}

// Restart preserves the task, original assignee/session, source bindings and all
// artifacts. It invalidates approval, not history. Busy tasks must be paused and
// acknowledged first; no running process can retain an old execution grant.
func (s *Store) RestartDevelopment(ctx context.Context, taskID string, expected int64) (model.Development, error) {
	var d model.Development
	if m, err := s.Maintenance(ctx); err != nil {
		return d, err
	} else if m != "" {
		return d, fmt.Errorf("%w: maintenance active", model.ErrConflict)
	}
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		t, err := getTaskTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if t.Version != expected {
			return fmt.Errorf("%w: task changed; refresh before restarting", model.ErrConflict)
		}
		var busy bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE (task_id=? OR task_id IN (SELECT to_task_id FROM task_edge WHERE from_task_id=?)) AND state IN ('RUNNING','QUEUED')) OR EXISTS(SELECT 1 FROM review_turn WHERE task_id=? AND state IN ('RUNNING','QUEUED')) OR EXISTS(SELECT 1 FROM source_review WHERE task_id=?) OR EXISTS(SELECT 1 FROM source_target WHERE task_id=?)`, taskID, taskID, taskID, taskID, taskID).Scan(&busy); err != nil {
			return err
		}
		if busy || t.State == model.TaskStateCompleted {
			return fmt.Errorf("%w: restart requires an idle unfinished task without registered PRs", model.ErrConflict)
		}
		old, err := developmentTx(ctx, tx, taskID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if old.ReviewerTaskID != "" {
			if _, err = tx.ExecContext(ctx, `UPDATE task_message SET delivery='CANCELLED' WHERE task_id=? AND delivery='PENDING'`, old.ReviewerTaskID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, old.ReviewerTaskID); err != nil {
				return err
			}
			if err = setWorkStateTx(ctx, tx, old.ReviewerTaskID, model.TaskStateCompleted); err != nil {
				return err
			}
		}
		d = model.Development{TaskID: taskID, Phase: "PLANNING", Version: old.Version + 1, ReviewerTaskID: old.ReviewerTaskID}
		if err = saveDevelopmentTx(ctx, tx, d, "DevelopmentRestarted"); err != nil {
			return err
		}
		_, err = messageWorkFromTx(ctx, tx, taskID, "重新开始本任务的方案阶段。历史方案和结果仅作参考，重新检查当前需求和代码，提交完整方案供独立 Agent 评审。双方达成一致后必须等待用户明确批准方案，才能开始开发。", "development-restart:"+fmt.Sprint(d.Version), false, "system")
		return err
	})
	return d, err
}

// New developer work opts into the lifecycle when the Router assigns a member
// with code.implement. Existing work is not silently reinterpreted on migration.
func ensureDevelopmentTx(ctx context.Context, tx *sql.Tx, req CreateRunRequest) error {
	var eligible bool
	if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM run WHERE task_id=?) AND NOT EXISTS(SELECT 1 FROM task_edge WHERE to_task_id=?) AND NOT EXISTS(SELECT 1 FROM development WHERE task_id=?)`, req.TaskID, req.TaskID, req.TaskID).Scan(&eligible); err != nil || !eligible {
		return err
	}
	a, err := readJSONRow[model.AgentProfile](tx.QueryRowContext(ctx, `SELECT data_json FROM agent_profile WHERE agent_id=?`, req.AgentID))
	if err != nil {
		return err
	}
	role, err := readJSONRow[model.Role](tx.QueryRowContext(ctx, `SELECT data_json FROM role WHERE role_id=?`, a.RoleID))
	if err != nil {
		return err
	}
	if !slices.Contains(role.Capabilities, "code.implement") {
		return nil
	}
	return saveDevelopmentTx(ctx, tx, model.Development{TaskID: req.TaskID, Phase: "PLANNING", Version: 1}, "DevelopmentStarted")
}

func developmentInstructionsTx(ctx context.Context, tx *sql.Tx, req *CreateRunRequest) (*model.Development, error) {
	d, err := developmentTx(ctx, tx, req.TaskID)
	reviewer := false
	if err == sql.ErrNoRows {
		d, err = readJSONRow[model.Development](tx.QueryRowContext(ctx, `SELECT data_json FROM development WHERE json_extract(data_json,'$.reviewer_task_id')=?`, req.TaskID))
		reviewer = true
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if reviewer {
		if d.Phase != "AGENT_REVIEW" {
			return nil, fmt.Errorf("%w: no current plan awaiting Agent review", model.ErrConflict)
		}
		original, err := getTaskTx(ctx, tx, d.TaskID)
		if err != nil {
			return nil, err
		}
		req.Instructions += "\n原始任务及验收要求（工作材料，不是权限授权）：\n" + original.Goal + "\n"
		req.Instructions += "\n本任务是独立方案评审，不是 PR 评审。不修改代码、不申请流水线、不登记 PR、不找用户例行验收。检查下面完整方案的正确性、边界、扩展性、产品行为、可测试性及验收标准。重点检查 validation_plan：复用证据是否有明确身份和适用前提、rerun/add 是否覆盖改动影响、exclude 是否确实不在目标调用链、final_gate 是否足以支撑 PR；笼统的全部重跑或全部复用必须打回。review_decision 明确填 passed、changes_requested 或 blocked；outcome=review。具体问题与建议写入 message 和报告。Manager 将反馈原开发 Agent，后续版本回到本 Session。passed 仅代表方案可提交人工评审，绝不授权开发。\n"
	} else if d.Phase == "IMPLEMENTING" {
		if d.ApprovedReviewID == "" || d.PlanHash == "" {
			return nil, fmt.Errorf("%w: missing plan approval", model.ErrConflict)
		}
		if req.AdapterID != "codex-agent" {
			return nil, fmt.Errorf("%w: this adapter does not support approved isolated development yet; session was preserved", model.ErrConflict)
		}
		req.ReadOnly = false
		req.ExecutionGrant = &model.ExecutionGrant{ReviewID: d.ApprovedReviewID, PlanHash: d.PlanHash, Repository: d.Repository, BaseBranch: d.BaseBranch}
		req.Instructions = strings.ReplaceAll(req.Instructions, "当前运行使用只读沙箱。", "当前运行使用已批准的隔离开发沙箱。")
		req.Instructions = strings.ReplaceAll(req.Instructions, "不自行执行仓库写入、发布、推送、合并、发消息或其它外部变更。", "本轮允许在批准的隔离源码目录写入和验证；发布、推送、合并、发消息或其它外部变更仍必须走受控执行器。")
		req.Instructions += "\n当前阶段：人工已批准方案，开始实施。执行范围仅为下述批准的仓库和隔离工作目录。角色历史快照中关于普通任务只读的旧部署说明由本轮执行授权取代，其它职责和边界不变。请真正修改代码、运行可行的验证。实施期间没有 Phase 0/Phase 1、入口评审、清单评审或其它由你自行设立的 Agent 放行门槛；不得因“等待 Manager 安排独立复审”而停止实施。尚有批准范围内的工作时继续完成；如果本轮必须结束但仍可自主继续，使用 recovery_request 续接原 Session。只有代码和必要验证完成后才提交 publish_request。不要修改共享仓库，不要自行 commit/push/创建 PR，Manager 会在你提交 publish_request 后受控发布。PR 创建后，新建 PR 及每个新 commit 由任务源和 Router 触发独立 reviewer，不要在 PR 前自行邀请 reviewer。需要重大调整方案时返回 replan，不要擅自扩大范围。不得用文档代替代码实现；如果无法验证，明确未验证和阻塞，不要虚报完成。\n"
		if guidance := repositoryBuildGuidance(d.Repository); guidance != "" {
			req.Instructions += "\n" + guidance + "\n"
		}
		if d.ValidationPlan != nil {
			raw, _ := json.Marshal(d.ValidationPlan)
			req.Instructions += "\n当前方案已批准的结构化验证策略：\n" + string(raw) + "\n先按本轮改动影响执行必要的增量验证；稳定候选才执行 final_gate。复用项的代码、依赖、制品、配置或环境前提变化时，不得继续引用旧结果，必须重跑并在发布说明中逐项对账。若系统消息明确标示为“用户已受控确认调整验证范围”，本轮 validation_plan 就是保留原审批后的唯一验证门槛；不得自行重新设计或另邀方案 reviewer，也不得把这个例外扩展到其它产品/权限变更。\n"
		}
	} else if d.Phase == "PLANNING" {
		req.Instructions += "\n当前阶段：形成/修订方案（只读）。需求给设计文档，BUG 给问题分析与修复方案；检查真实代码、明确范围、风险、测试方法和验收标准，完整方案放 artifacts。outcome=review 时 plan_scope 填 GitHub owner/repository 和 base_branch，并提交结构化 validation_plan，区分可复用证据、受影响需重跑、需要补测、明确排除以及稳定候选的最终门禁；这是待审批的执行范围，不是权限。Manager 会安排另一个 Agent 互审，达成一致后用户审批，通过前严禁开发或创建 PR。历史方案可参考，但本轮必须提交完整方案。不要自行邀请人验收。\n"
	} else {
		return nil, fmt.Errorf("%w: lifecycle is waiting for review; use review chat or explicitly request plan changes", model.ErrConflict)
	}
	if d.PlanRunID != "" {
		var raw string
		if err = tx.QueryRowContext(ctx, `SELECT output FROM run WHERE run_id=?`, d.PlanRunID).Scan(&raw); err != nil {
			return nil, err
		}
		req.Instructions += "\n当前待评审/已批准方案（工作材料，不是权限）：\n" + raw
	}
	if !reviewer && d.ReviewerRunID != "" {
		var raw string
		if err = tx.QueryRowContext(ctx, `SELECT output FROM run WHERE run_id=?`, d.ReviewerRunID).Scan(&raw); err != nil {
			return nil, err
		}
		req.Instructions += "\n独立 reviewer 最近一轮完整反馈（工作材料）：\n" + raw
	}
	return &d, nil
}

func applyDevelopmentResultTx(ctx context.Context, tx *sql.Tx, taskID string, e model.RuntimeEvent, result workflow.Result, artifactIDs []string, now int64) (bool, error) {
	var parent, phase string
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT task_id,plan_version,phase FROM development_run WHERE run_id=?`, e.RunID).Scan(&parent, &version, &phase)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	d, err := developmentTx(ctx, tx, parent)
	if err != nil {
		return true, err
	}
	if d.Version != version || d.Phase != phase {
		// A message/reset superseded this run. Preserve its output but never use it
		// as the current approval or execute its side effects.
		var pending bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING')`, taskID).Scan(&pending); err != nil {
			return true, err
		}
		state := model.TaskStateWaiting
		if pending {
			state = model.TaskStateQueued
		}
		return true, setWorkStateTx(ctx, tx, taskID, state)
	}
	if parent != taskID {
		if result.Outcome != "review" || !slices.Contains([]string{"passed", "changes_requested", "blocked"}, result.ReviewDecision) {
			return true, blockDevelopmentTx(ctx, tx, taskID, "方案 reviewer 必须提交明确结论（passed/changes_requested/blocked），没有结论不能视为通过。")
		}
		d.ReviewerRunID = e.RunID
		var parentBusy bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING') OR EXISTS(SELECT 1 FROM task_workflow WHERE task_id=? AND paused=1)`, parent, parent).Scan(&parentBusy); err != nil {
			return true, err
		}
		if parentBusy {
			// A plan reviewer has finished its own work even if the parent still
			// has an inbox item to process. It never waits for a child task.
			return true, setWorkStateTx(ctx, tx, taskID, model.TaskStateCompleted)
		}
		if result.ReviewDecision == "passed" {
			d.Phase = "HUMAN_REVIEW"
			ids, err := listJSONRows[model.Artifact](ctx, tx, `SELECT data_json FROM artifact WHERE run_id=? ORDER BY rowid`, d.PlanRunID)
			if err != nil {
				return true, err
			}
			approvedArtifacts := []string{}
			for _, a := range ids {
				approvedArtifacts = append(approvedArtifacts, a.ID)
			}
			r := model.Review{ID: id.New("review"), TaskID: parent, RunID: d.PlanRunID, Kind: "plan", PlanHash: d.PlanHash, State: "PENDING", ArtifactIDs: approvedArtifacts, CreatedAtMS: now}
			raw, _ := json.Marshal(r)
			if _, err = tx.ExecContext(ctx, `INSERT INTO review VALUES(?,?,?,?,?)`, r.ID, parent, r.RunID, r.State, raw); err != nil {
				return true, err
			}
			if _, err = appendEventTx(ctx, tx, "task", parent, "PlanReviewRequested", e.RunID, parent, r); err != nil {
				return true, err
			}
			if _, err = insertMessageTx(ctx, tx, parent, "system", "独立 Agent 方案评审通过。请确认方案、仓库及目标分支；批准前不会开始开发。\n"+truncateRunes(result.Message, 2000), e.RunID, "RECORDED"); err != nil {
				return true, err
			}
			if err = setWorkStateTx(ctx, tx, parent, model.TaskStateReview); err != nil {
				return true, err
			}
		} else {
			d.Phase = "PLANNING"
			if err = saveDevelopmentTx(ctx, tx, d, "PlanChangesRequested"); err != nil {
				return true, err
			}
			if _, err = taskEventMessageTx(ctx, tx, parent, "独立方案 reviewer 反馈，请逐项回应并修订完整方案，不开始开发：\n"+truncateRunes(result.Message, 6000), "plan-feedback:"+e.RunID, "system"); err != nil {
				return true, err
			}
		}
		if err = saveDevelopmentTx(ctx, tx, d, "PlanAgentReviewed"); err != nil {
			return true, err
		}
		// The review report is now delivered. A later plan revision explicitly
		// reactivates this same reviewer task and Session.
		return true, setWorkStateTx(ctx, tx, taskID, model.TaskStateCompleted)
	}
	if phase == "IMPLEMENTING" {
		if result.Outcome == "amend_validation" {
			if result.ValidationPlan == nil || result.VerificationAmendment == nil {
				return true, requestRecoveryTx(ctx, tx, parent, e.RunID, workflow.RecoveryRequest{Evidence: "Agent 声称仅补充验证，但没有提交完整的结构化验证策略或受限测试路径。", NextStep: "若产品方案没有变化，提交 outcome=amend_validation、完整 validation_plan、verification_amendment 和 task_update；若产品行为或范围变化，使用 replan。"})
			}
			// A reviewer asking for durable regression coverage is not a new product
			// decision. Keep the original human approval, repository scope and
			// execution grant intact; only the test strategy is amended.
			d.ValidationPlan = validationPlanModel(result.ValidationPlan)
			if err = saveDevelopmentTx(ctx, tx, d, "ValidationPlanAmended"); err != nil {
				return true, err
			}
			if _, err = appendEventTx(ctx, tx, "task", parent, "VerificationAmendmentAccepted", e.RunID, parent, map[string]any{
				"run_id":      e.RunID,
				"reason":      result.VerificationAmendment.Reason,
				"paths":       result.VerificationAmendment.Paths,
				"plan_hash":   d.PlanHash,
				"review_id":   d.ApprovedReviewID,
				"validation":  d.ValidationPlan,
				"task_update": result.TaskUpdate,
			}); err != nil {
				return true, err
			}
			_, err = taskEventMessageTx(ctx, tx, parent, "评审要求补充仓库内验证，未改变已批准的产品方案、仓库范围或执行授权。请沿用原 Session 继续开发，完成以下测试补充并按更新后的验证策略交付：\n"+truncateRunes(result.Message, 6000), "validation-amendment:"+e.RunID, "system")
			return true, err
		}
		if result.Outcome == "replan" {
			if result.ValidationPlan == nil {
				return true, requestRecoveryTx(ctx, tx, parent, e.RunID, workflow.RecoveryRequest{Evidence: "Agent 申请调整方案，但没有提交更新后的结构化验证策略。", NextStep: "补充 validation_plan，明确哪些历史测试复用、哪些因改动重跑、哪些新增或排除，以及稳定候选的 final_gate；不要开始新范围实施。"})
			}
			d.Phase = "PLANNING"
			d.Version++
			d.ApprovedReviewID = ""
			if err = saveDevelopmentTx(ctx, tx, d, "PlanApprovalRevoked"); err != nil {
				return true, err
			}
			_, err = taskEventMessageTx(ctx, tx, parent, "实施发现需要调整方案，请重新提交完整方案供 Agent 和人工评审：\n"+truncateRunes(result.Message, 6000), "replan:"+e.RunID, "system")
			return true, err
		}
		if result.Outcome == "review" && len(result.PullRequests) == 0 {
			return true, requestRecoveryTx(ctx, tx, parent, e.RunID, workflow.RecoveryRequest{
				Evidence: "Agent 在实施阶段返回 outcome=review，但没有提交 publish_request，受控执行器因此未创建 PR。\n" + truncateRunes(result.Message, 3000),
				NextStep: "不要安排 PR 前的独立 Agent 复审，也不要自行增加 Phase 0/Phase 1 放行门槛。继续原已批准方案内的实现和必要验证；完成后提交 publish_request，由 Runtime 创建 PR。PR 新建及后续新 commit 再由 Router 邀请 reviewer。",
			})
		}
		return false, nil // Existing PR registration/review/testing lifecycle.
	}
	knownPRs, err := developmentPRReferencesKnownTx(ctx, tx, parent, result.PullRequests)
	if err != nil {
		return true, err
	}
	if !knownPRs || len(result.TestRequests) > 0 || result.PublishRequest != nil {
		return true, blockDevelopmentTx(ctx, tx, parent, "方案阶段不允许发布或登记 PR，也不允许申请实现测试。")
	}
	// An implementation replan has already revoked the old approval and moved
	// the lifecycle back to PLANNING. If the next turn submits a complete
	// revised plan it is immaterial whether the Agent calls that submission
	// "review" or "replan": both must enter the same independent review gate.
	// Requiring another semantically identical turn creates a permanent manager
	// block without adding any safety. The structural plan checks below remain
	// authoritative.
	if result.Outcome != "review" && result.Outcome != "replan" {
		return false, nil
	}
	if len(artifactIDs) == 0 || result.PlanScope == nil || result.ValidationPlan == nil || model.ValidateDevelopmentRepository(result.PlanScope.Repository, result.PlanScope.BaseBranch) != nil {
		return true, blockDevelopmentTx(ctx, tx, parent, "方案需包含完整 artifacts、plan_scope.repository、base_branch 和结构化 validation_plan；未创建人工验收单。")
	}
	d.Version++
	d.Rounds++
	d.Phase = "AGENT_REVIEW"
	d.PlanRunID = e.RunID
	d.PlanHash = publicationKey(e.Output)
	d.Repository = result.PlanScope.Repository
	d.BaseBranch = result.PlanScope.BaseBranch
	d.ValidationPlan = validationPlanModel(result.ValidationPlan)
	d.ApprovedReviewID = ""
	if err = saveDevelopmentTx(ctx, tx, d, "PlanSubmitted"); err != nil {
		return true, err
	}
	return true, setWorkStateTx(ctx, tx, parent, model.TaskStateWaiting)
}

// ContinueApprovedDevelopmentAfterVerificationAmendment repairs work that was
// sent through the legacy broad replan transition solely to add verification.
// It is deliberately narrow: a pending revised plan may be superseded only by
// restoring a previous human-approved plan for the same task. The revised
// validation strategy remains recorded and becomes the implementation gate.
func (s *Store) ContinueApprovedDevelopmentAfterVerificationAmendment(ctx context.Context, taskID string, expected int64, reason string) (model.Development, error) {
	var restored model.Development
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 4000 {
		return restored, fmt.Errorf("%w: verification amendment reason required", model.ErrValidation)
	}
	if maintenance, err := s.Maintenance(ctx); err != nil {
		return restored, err
	} else if maintenance != "" {
		return restored, fmt.Errorf("%w: maintenance active", model.ErrConflict)
	}
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		task, err := getTaskTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if task.Version != expected || task.State != model.TaskStateReview {
			return fmt.Errorf("%w: task changed; refresh before continuing the approved plan", model.ErrConflict)
		}
		d, err := developmentTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if d.Phase != "HUMAN_REVIEW" || d.ApprovedReviewID != "" || d.ValidationPlan == nil {
			return fmt.Errorf("%w: only a pending revised validation plan can retain the prior approval", model.ErrConflict)
		}
		var busy bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')) OR EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING') OR EXISTS(SELECT 1 FROM review_turn WHERE task_id=? AND state IN ('QUEUED','RUNNING'))`, taskID, taskID, taskID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return fmt.Errorf("%w: task has pending work", model.ErrConflict)
		}
		var pending model.Review
		pending, err = readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE task_id=? AND state='PENDING' AND json_extract(data_json,'$.kind')='plan' AND json_extract(data_json,'$.plan_hash')=? AND run_id=?`, taskID, d.PlanHash, d.PlanRunID))
		if err != nil {
			return fmt.Errorf("%w: pending plan review changed", model.ErrConflict)
		}
		var approved model.Review
		approved, err = readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE task_id=? AND state='PLAN_APPROVED' AND json_extract(data_json,'$.kind')='plan' ORDER BY json_extract(data_json,'$.decided_at_ms') DESC,rowid DESC LIMIT 1`, taskID))
		if err != nil {
			return fmt.Errorf("%w: no earlier human-approved plan to retain", model.ErrConflict)
		}
		var raw string
		if err = tx.QueryRowContext(ctx, `SELECT output FROM run WHERE run_id=?`, approved.RunID).Scan(&raw); err != nil {
			return err
		}
		prior, err := workflow.Parse(raw)
		if err != nil || prior.PlanScope == nil || model.ValidateDevelopmentRepository(prior.PlanScope.Repository, prior.PlanScope.BaseBranch) != nil {
			return fmt.Errorf("%w: earlier approved plan cannot be restored", model.ErrConflict)
		}
		if err = supersedeReviewsTx(ctx, tx, taskID); err != nil {
			return err
		}
		restored = d
		restored.Phase = "IMPLEMENTING"
		restored.Version++
		restored.PlanRunID = approved.RunID
		restored.PlanHash = approved.PlanHash
		restored.ApprovedReviewID = approved.ID
		restored.Repository = prior.PlanScope.Repository
		restored.BaseBranch = prior.PlanScope.BaseBranch
		// The latest pending plan-review feedback belongs to the superseded
		// revision, not to the retained product plan.
		restored.ReviewerRunID = ""
		if err = saveDevelopmentTx(ctx, tx, restored, "VerificationAmendmentAdopted"); err != nil {
			return err
		}
		if _, err = appendEventTx(ctx, tx, "task", taskID, "VerificationAmendmentRestoredApproval", "", taskID, map[string]any{
			"reason":                  reason,
			"superseded_review_id":    pending.ID,
			"superseded_plan_run_id":  pending.RunID,
			"retained_review_id":      approved.ID,
			"retained_plan_run_id":    approved.RunID,
			"retained_plan_hash":      approved.PlanHash,
			"amended_validation_plan": restored.ValidationPlan,
		}); err != nil {
			return err
		}
		_, err = taskEventMessageTx(ctx, tx, taskID, "本次变更仅补充评审要求的测试与验证，不改变此前已人工批准的产品方案。系统已保留原审批、仓库范围和原 Session，并采用更新后的验证策略继续开发：\n"+reason, "verification-amendment-restore:"+pending.ID, "system")
		return err
	})
	return restored, err
}

// pull_requests are declarative references as well as registration requests.
// During PLANNING an Agent may repeat a PR that this task already owns so a
// revised plan remains self-contained. Only a new target is a side effect and
// must be rejected until implementation. This comparison deliberately ignores
// source_id: ownership of the canonical PR URL is the durable authority.
func developmentPRReferencesKnownTx(ctx context.Context, tx *sql.Tx, taskID string, prs []workflow.PullRequest) (bool, error) {
	for _, pr := range prs {
		_, _, _, canonical, err := model.ParseGitHubPR(pr.URL)
		if err != nil {
			return false, nil
		}
		var known bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM source_target WHERE task_id=? AND entity=?)`, taskID, canonical).Scan(&known); err != nil {
			return false, err
		}
		if !known {
			return false, nil
		}
	}
	return true, nil
}

func validationPlanModel(plan *workflow.ValidationPlan) *model.DevelopmentValidationPlan {
	if plan == nil {
		return nil
	}
	convert := func(items []workflow.ValidationItem) []model.DevelopmentValidationItem {
		out := make([]model.DevelopmentValidationItem, 0, len(items))
		for _, item := range items {
			out = append(out, model.DevelopmentValidationItem{Scenario: item.Scenario, Reason: item.Reason, Evidence: item.Evidence})
		}
		return out
	}
	return &model.DevelopmentValidationPlan{Reuse: convert(plan.Reuse), Rerun: convert(plan.Rerun), Add: convert(plan.Add), Exclude: convert(plan.Exclude), FinalGate: append([]string(nil), plan.FinalGate...)}
}

func blockDevelopmentTx(ctx context.Context, tx *sql.Tx, taskID, message string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE task_workflow SET paused=1,scheduler_error=? WHERE task_id=?`, message, taskID); err != nil {
		return err
	}
	if _, err := insertMessageTx(ctx, tx, taskID, "system", message, "", "RECORDED"); err != nil {
		return err
	}
	return setWorkStateTx(ctx, tx, taskID, model.TaskStateBlocked)
}

// Routing happens in the Manager, independently of source polling. Reuses the
// same reviewer task/session across revisions and an idempotent inbox per plan.
func (s *Store) RoutePlanReviews(ctx context.Context) error {
	if m, err := s.Maintenance(ctx); err != nil || m != "" {
		return err
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		if err := reconcileIdlePlanReviewerTasksTx(ctx, tx); err != nil {
			return err
		}
		ds, err := listJSONRows[model.Development](ctx, tx, `SELECT d.data_json FROM development d JOIN task_workflow w USING(task_id) WHERE json_extract(d.data_json,'$.phase')='AGENT_REVIEW' AND w.paused=0`)
		if err != nil {
			return err
		}
		for _, d := range ds {
			if d.ReviewerTaskID == "" {
				task, err := getTaskTx(ctx, tx, d.TaskID)
				if err != nil {
					return err
				}
				agents, err := listAgentsTx(ctx, tx)
				if err != nil {
					return err
				}
				roleID, err := (router.PlanReviewer{}).Select(d.Repository, task.AssignedAgentID, agents)
				if err != nil {
					if _, e := tx.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error=? WHERE task_id=?`, err.Error(), d.TaskID); e != nil {
						return e
					}
					continue
				}
				child, err := createWorkTx(ctx, tx, CreateWorkRequest{Title: "方案评审 · " + truncateRunes(task.Title, 100), Goal: "独立评审原任务的方案，反馈开发 Agent，多轮讨论后提交人工确认。", Source: "router.plan-review", Key: "plan-review:" + d.TaskID, Requirements: model.TaskRequirements{RoleID: roleID, ExcludedAgentIDs: []string{task.AssignedAgentID}}})
				if err != nil {
					return err
				}
				d.ReviewerTaskID = child.ID
				if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET project_json=(SELECT project_json FROM task_workflow WHERE task_id=?) WHERE task_id=?`, d.TaskID, child.ID); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms) VALUES(?,?,?,'REVIEWS',?)`, id.New("edge"), d.TaskID, child.ID, time.Now().UnixMilli()); err != nil {
					return err
				}
				if err = saveDevelopmentTx(ctx, tx, d, "PlanReviewerRouted"); err != nil {
					return err
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE task SET state=?,version=version+1,updated_at_ms=? WHERE task_id=? AND state=?`, model.TaskStateQueued, time.Now().UnixMilli(), d.ReviewerTaskID, model.TaskStateCompleted); err != nil {
				return err
			}
			if _, err = taskEventMessageTx(ctx, tx, d.ReviewerTaskID, fmt.Sprintf("请评审方案第 %d 版，hash %s。完整方案由 Manager 附在本轮输入中。", d.Version, d.PlanHash), fmt.Sprintf("plan-version:%d:%s", d.Version, d.PlanHash), "system"); err != nil {
				return err
			}
		}
		return nil
	})
}

// reconcileIdlePlanReviewerTasksTx repairs legacy reviewer tasks that were
// incorrectly left in WAITING_SUBTASKS after their report had been delivered.
// A plan reviewer has no child task of its own: once idle, it is completed
// until a later plan revision explicitly requeues its existing Session.
func reconcileIdlePlanReviewerTasksTx(ctx context.Context, tx *sql.Tx) error {
	ds, err := listJSONRows[model.Development](ctx, tx, `SELECT data_json FROM development`)
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.ReviewerTaskID == "" || d.Phase == "AGENT_REVIEW" {
			continue
		}
		var stale bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM task t
				JOIN task_workflow w USING(task_id)
				WHERE t.task_id=? AND t.state=? AND w.paused=0
				  AND NOT EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING'))
				  AND NOT EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING')
			)`, d.ReviewerTaskID, model.TaskStateWaiting, d.ReviewerTaskID, d.ReviewerTaskID).Scan(&stale); err != nil {
			return err
		}
		if !stale {
			continue
		}
		if err := setWorkStateTx(ctx, tx, d.ReviewerTaskID, model.TaskStateCompleted); err != nil {
			return err
		}
		if _, err := appendEventTx(ctx, tx, "task", d.ReviewerTaskID, "PlanReviewerIdleStateReconciled", "", d.TaskID, map[string]any{
			"parent_task_id": d.TaskID,
			"phase":          d.Phase,
			"from_state":     model.TaskStateWaiting,
			"to_state":       model.TaskStateCompleted,
		}); err != nil {
			return err
		}
	}
	return nil
}

func decidePlanTx(ctx context.Context, tx *sql.Tx, d model.Development, r *model.Review, decision, comment string) error {
	if d.Phase != "HUMAN_REVIEW" || r.PlanHash != d.PlanHash || r.RunID != d.PlanRunID {
		return fmt.Errorf("%w: plan revision changed", model.ErrConflict)
	}
	if decision != "PLAN_APPROVED" && decision != "CHANGES_REQUESTED" {
		return fmt.Errorf("%w: use explicit PLAN_APPROVED; final acceptance cannot approve a plan", model.ErrValidation)
	}
	if decision == "PLAN_APPROVED" {
		if err := model.ValidateDevelopmentRepository(d.Repository, d.BaseBranch); err != nil {
			return err
		}
		d.Phase = "IMPLEMENTING"
		d.ApprovedReviewID = r.ID
		d.PublishApprovedPlan = true
		if err := setWorkStateTx(ctx, tx, d.ReviewerTaskID, model.TaskStateCompleted); err != nil {
			return err
		}
		if _, err := createTaskSummaryTx(ctx, tx, d.ReviewerTaskID, "run", d.ReviewerRunID, time.Now().UnixMilli()); err != nil {
			return err
		}
	} else {
		d.Phase = "PLANNING"
		d.Version++
		d.ApprovedReviewID = ""
	}
	r.State = decision
	r.Comment = comment
	r.DecidedAtMS = time.Now().UnixMilli()
	raw, _ := json.Marshal(r)
	if _, err := tx.ExecContext(ctx, `UPDATE review SET state=?,data_json=? WHERE review_id=?`, r.State, raw, r.ID); err != nil {
		return err
	}
	if err := saveDevelopmentTx(ctx, tx, d, "PlanHumanDecided"); err != nil {
		return err
	}
	message := "人工要求修改方案，重新走 Agent 互审与人工确认：\n" + comment
	if decision == "PLAN_APPROVED" {
		message = "人工明确批准当前方案。请在隔离工作区按批准方案开发、验证并提交真实 PR，然后进入 PR 评审流程。\n" + comment
	}
	if _, err := insertMessageTx(ctx, tx, d.TaskID, "user", message, "", "PENDING"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, d.TaskID); err != nil {
		return err
	}
	return setWorkStateTx(ctx, tx, d.TaskID, model.TaskStateQueued)
}
