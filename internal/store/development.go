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
			if err = setWorkStateTx(ctx, tx, old.ReviewerTaskID, model.TaskStateWaiting); err != nil {
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
		req.Instructions += "\n本任务是独立方案评审，不是 PR 评审。不修改代码、不申请流水线、不登记 PR、不找用户例行验收。检查下面完整方案的正确性、边界、扩展性、产品行为、可测试性及验收标准。review_decision 明确填 passed、changes_requested 或 blocked；outcome=review。具体问题与建议写入 message 和报告。Manager 将反馈原开发 Agent，后续版本回到本 Session。passed 仅代表方案可提交人工评审，绝不授权开发。\n"
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
		req.Instructions += "\n当前阶段：人工已批准方案，开始实施。执行范围仅为下述批准的仓库和隔离工作目录。角色历史快照中关于普通任务只读的旧部署说明由本轮执行授权取代，其它职责和边界不变。请真正修改代码、运行可行的验证。不要修改共享仓库，不要自行 commit/push/创建 PR，Manager 会在你提交 publish_request 后受控发布。需要重大调整方案时返回 replan，不要擅自扩大范围。不得用文档代替代码实现；如果无法验证，明确未验证和阻塞，不要虚报完成。\n"
	} else if d.Phase == "PLANNING" {
		req.Instructions += "\n当前阶段：形成/修订方案（只读）。需求给设计文档，BUG 给问题分析与修复方案；检查真实代码、明确范围、风险、测试方法和验收标准，完整方案放 artifacts。outcome=review 时 plan_scope 填 GitHub owner/repository 和 base_branch；这是待审批的执行范围，不是权限。Manager 会安排另一个 Agent 互审，达成一致后用户审批，通过前严禁开发或创建 PR。历史方案可参考，但本轮必须提交完整方案。不要自行邀请人验收。\n"
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
			return true, setWorkStateTx(ctx, tx, taskID, model.TaskStateWaiting)
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
		return true, setWorkStateTx(ctx, tx, taskID, model.TaskStateWaiting)
	}
	if phase == "IMPLEMENTING" {
		if result.Outcome == "replan" {
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
			return true, blockDevelopmentTx(ctx, tx, parent, "开发交付缺少真实 PR，不能把文档当作开发完成。请检查发布状态并继续处理。")
		}
		return false, nil // Existing PR registration/review/testing lifecycle.
	}
	if len(result.PullRequests) > 0 || len(result.TestRequests) > 0 || result.PublishRequest != nil {
		return true, blockDevelopmentTx(ctx, tx, parent, "方案阶段不允许发布或登记 PR，也不允许申请实现测试。")
	}
	if result.Outcome != "review" {
		return false, nil
	}
	if len(artifactIDs) == 0 || result.PlanScope == nil || model.ValidateDevelopmentRepository(result.PlanScope.Repository, result.PlanScope.BaseBranch) != nil {
		return true, blockDevelopmentTx(ctx, tx, parent, "方案需包含完整 artifacts、plan_scope.repository 和 base_branch；未创建人工验收单。")
	}
	if len(result.PullRequests) > 0 || len(result.TestRequests) > 0 {
		return true, blockDevelopmentTx(ctx, tx, parent, "方案阶段不允许登记 PR 或申请实现测试。")
	}
	d.Version++
	d.Rounds++
	d.Phase = "AGENT_REVIEW"
	d.PlanRunID = e.RunID
	d.PlanHash = publicationKey(e.Output)
	d.Repository = result.PlanScope.Repository
	d.BaseBranch = result.PlanScope.BaseBranch
	d.ApprovedReviewID = ""
	if err = saveDevelopmentTx(ctx, tx, d, "PlanSubmitted"); err != nil {
		return true, err
	}
	return true, setWorkStateTx(ctx, tx, parent, model.TaskStateWaiting)
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
		ds, err := listJSONRows[model.Development](ctx, tx, `SELECT d.data_json FROM development d JOIN task_workflow w USING(task_id) WHERE json_extract(d.data_json,'$.phase')='AGENT_REVIEW' AND w.paused=0`)
		if err != nil {
			return err
		}
		for _, d := range ds {
			if d.Rounds > 8 {
				if err = blockDevelopmentTx(ctx, tx, d.TaskID, "方案互审达到自动轮次上限，请查看分歧后决定方向。"); err != nil {
					return err
				}
				continue
			}
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
				child, err := createWorkTx(ctx, tx, CreateWorkRequest{Title: "方案评审 · " + task.Title, Goal: "独立评审原任务的方案，反馈开发 Agent，多轮讨论后提交人工确认。", Source: "router.plan-review", Key: "plan-review:" + d.TaskID, Requirements: model.TaskRequirements{RoleID: roleID, ExcludedAgentIDs: []string{task.AssignedAgentID}}})
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
			if _, err = tx.ExecContext(ctx, `UPDATE task SET state='WAITING_SUBTASKS',version=version+1 WHERE task_id=? AND state='COMPLETED'`, d.ReviewerTaskID); err != nil {
				return err
			}
			if _, err = taskEventMessageTx(ctx, tx, d.ReviewerTaskID, fmt.Sprintf("请评审方案第 %d 版，hash %s。完整方案由 Manager 附在本轮输入中。", d.Version, d.PlanHash), "plan-version:"+d.PlanHash, "system"); err != nil {
				return err
			}
		}
		return nil
	})
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
