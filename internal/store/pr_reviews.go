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
	"work-assistant/internal/router"
	"work-assistant/internal/workflow"
)

func createSourceReviewsTx(ctx context.Context, tx *sql.Tx, planner router.ReviewPlanner, target model.SourceTarget, event model.SourceEvent) error {
	task, err := getTaskTx(ctx, tx, target.TaskID)
	if err != nil {
		return err
	}
	if task.State == model.TaskStateCompleted {
		return nil
	}
	owner, repo, _, _, err := model.ParseGitHubPR(target.Entity)
	if err != nil {
		return err
	}
	if !model.CommitSHA.MatchString(event.HeadSHA) {
		return fmt.Errorf("%w: invalid review revision", model.ErrValidation)
	}
	agents, err := listAgentsTx(ctx, tx)
	if err != nil {
		return err
	}
	request := router.ReviewRequest{Repository: repo, AuthorAgentID: task.AssignedAgentID}
	plan, err := planner.PlanReviews(request, agents)
	if err != nil {
		return err
	}
	policy, err := optimizationPolicyForTaskTx(ctx, tx, task.ID)
	if err != nil {
		return err
	}
	if limit := policy.Test.ReviewerCount; limit > 0 && len(plan.RoleIDs) > limit {
		plan.RoleIDs = append([]string{}, plan.RoleIDs[:limit]...)
		plan.Reason += fmt.Sprintf(" 当前任务固定策略将本次评审限制为 %d 个角色。", limit)
	}
	if err = router.ValidateReviewPlan(request, plan, agents); err != nil {
		return err
	}
	parentWork, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, task.ID))
	if err != nil {
		return err
	}
	// Keep old results as history, but prevent stale reviews from influencing
	// the current revision or blocking its human acceptance.
	rows, err := tx.QueryContext(ctx, `SELECT task_id FROM source_review WHERE target_id=? AND head_sha!=? AND state!='SUPERSEDED'`, target.ID, event.HeadSHA)
	if err != nil {
		return err
	}
	var old []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		old = append(old, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, taskID := range old {
		t, err := getTaskTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if t.State != model.TaskStateCompleted {
			if err = pauseWorkTx(ctx, tx, taskID); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE source_review SET state='SUPERSEDED' WHERE target_id=? AND head_sha!=?`, target.ID, event.HeadSHA); err != nil {
		return err
	}
	for _, roleID := range plan.RoleIDs {
		needs := model.TaskRequirements{RoleID: roleID}
		if task.AssignedAgentID != "" {
			needs.ExcludedAgentIDs = []string{task.AssignedAgentID}
		}
		role, err := readJSONRow[model.Role](tx.QueryRowContext(ctx, `SELECT data_json FROM role WHERE role_id=?`, roleID))
		if err != nil {
			return err
		}
		brief := sourceReviewBrief(target, event.HeadSHA, role.Name)
		child, err := createWorkTx(ctx, tx, CreateWorkRequest{Title: brief.Title, Goal: brief.Goal, TaskType: "review", Repository: owner + "/" + repo, WorkflowType: "standard", Key: event.ID + ":" + roleID, Source: "router.review", Requirements: needs})
		if err != nil {
			return err
		}
		// Inherit the original task's frozen team context, not a mutable source
		// setting or a newer team-library version.
		project, err := json.Marshal(parentWork.Project)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET project_json=? WHERE task_id=?`, project, child.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO source_review(target_id,head_sha,role_id,task_id,state) VALUES(?,?,?,?,'PENDING')`, target.ID, event.HeadSHA, roleID, child.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms) VALUES(?,?,?,'REVIEWS',?)`, id.New("edge"), target.TaskID, child.ID, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if err = supersedeReviewsTx(ctx, tx, target.TaskID); err != nil {
		return err
	}
	var idle bool
	if err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('RUNNING','QUEUED')) AND NOT EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING') AND EXISTS(SELECT 1 FROM task_workflow WHERE task_id=? AND paused=0)`, target.TaskID, target.TaskID, target.TaskID).Scan(&idle); err != nil {
		return err
	}
	if idle {
		if err = setWorkStateTx(ctx, tx, target.TaskID, model.TaskStateWaiting); err != nil {
			return err
		}
	}
	if _, err = appendEventTx(ctx, tx, "task", target.TaskID, "PRReviewRouted", event.ID, target.TaskID, map[string]any{"head_sha": event.HeadSHA, "target_id": target.ID, "plan": plan}); err != nil {
		return err
	}
	_, err = insertMessageTx(ctx, tx, target.TaskID, "system", fmt.Sprintf("PR 新版本 %s 已登记，Router 已安排 %d 项独立 Agent 评审。%s\nPR：%s", event.HeadSHA, len(plan.RoleIDs), plan.Reason, target.Entity), "", "RECORDED")
	return err
}

// Agent reviewers finish their internal evidence-producing work automatically.
// They do NOT approve the original task or perform external mutations.
func applySourceReviewResultTx(ctx context.Context, tx *sql.Tx, taskID string, e model.RuntimeEvent, result workflow.Result, now int64) (bool, error) {
	var targetID, head, state string
	err := tx.QueryRowContext(ctx, `SELECT target_id,head_sha,state FROM source_review WHERE task_id=?`, taskID).Scan(&targetID, &head, &state)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if state == "SUPERSEDED" {
		return true, setWorkStateTx(ctx, tx, taskID, model.TaskStatePaused)
	}
	if result.ReviewDecision == "waiting_tests" {
		if _, err = tx.ExecContext(ctx, `UPDATE source_review SET state='WAITING_TESTS' WHERE task_id=?`, taskID); err != nil {
			return true, err
		}
		return true, setWorkStateTx(ctx, tx, taskID, model.TaskStateWaiting)
	}
	if result.ReviewDecision == "blocked" || result.ReviewDecision == "changes_requested" {
		target, err := sourceTargetTx(ctx, tx, targetID)
		if err != nil {
			return true, err
		}
		if _, err = taskEventMessageTx(ctx, tx, target.TaskID, "PR reviewer 反馈（不是人工批准）：\n"+target.Entity+"\n版本 "+head+"\n"+truncateRunes(result.Message, 6000), "review-feedback:"+e.RunID, "system"); err != nil {
			return true, err
		}
	}
	if result.Outcome != "review" {
		return false, nil
	}
	if err = setWorkStateTx(ctx, tx, taskID, model.TaskStateCompleted); err != nil {
		return true, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE source_review SET state='COMPLETED' WHERE task_id=?`, taskID); err != nil {
		return true, err
	}
	if _, err = createTaskSummaryTx(ctx, tx, taskID, "run", e.RunID, now); err != nil {
		return true, err
	}
	return true, nil
}

// Fan-in is retried separately from run completion, so a large/paused author's
// inbox cannot prevent acknowledging a reviewer's completed runtime event.
func (s *Store) CollectSourceReviews(ctx context.Context) error {
	maintenance, err := s.Maintenance(ctx)
	if err != nil || maintenance != "" {
		return err
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT target_id,head_sha FROM source_review GROUP BY target_id,head_sha HAVING MIN(state='COMPLETED')=1 AND MIN(feedback_sent)=0 LIMIT 50`)
		if err != nil {
			return err
		}
		type batch struct{ target, head string }
		var batches []batch
		for rows.Next() {
			var b batch
			if err = rows.Scan(&b.target, &b.head); err != nil {
				rows.Close()
				return err
			}
			batches = append(batches, b)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, b := range batches {
			t, err := sourceTargetTx(ctx, tx, b.target)
			if err != nil {
				return err
			}
			reviews, err := tx.QueryContext(ctx, `SELECT r.task_id,t.title,(SELECT output FROM run WHERE task_id=r.task_id AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1) FROM source_review r JOIN task t USING(task_id) WHERE target_id=? AND head_sha=? ORDER BY role_id`, b.target, b.head)
			if err != nil {
				return err
			}
			var body strings.Builder
			fmt.Fprintf(&body, "PR %s\n版本 %s 的 Agent 评审已全部完成。请结合各项报告处理或回复；这不是人工批准。\n", t.Entity, b.head)
			for reviews.Next() {
				var taskID, title, output string
				if err = reviews.Scan(&taskID, &title, &output); err != nil {
					reviews.Close()
					return err
				}
				result, e := workflow.Parse(output)
				if e != nil {
					reviews.Close()
					return e
				}
				fmt.Fprintf(&body, "\n%s\n评审任务：%s\n%s\n", title, taskID, truncateRunes(result.Message, 700))
				for _, a := range result.Artifacts {
					fmt.Fprintf(&body, "报告 %s：\n%s\n", a.Name, truncateRunes(a.Content, 300))
				}
			}
			err = reviews.Err()
			reviews.Close()
			if err != nil {
				return err
			}
			message := truncateRunes(body.String(), 9000)
			if len(message) > 31000 {
				message = truncateRunes(message, 7000)
			}
			// This is an internal Manager result, not a new GitHub observation.
			// Deliver atomically with the fan-in marker, even if polling is off.
			if t.HeadSHA == b.head {
				if _, err = taskEventMessageTx(ctx, tx, t.TaskID, message, "review-results:"+t.ID+":"+b.head+":"+publicationKey(message), "system"); err != nil {
					return err
				}
				if _, err = appendEventTx(ctx, tx, "task", t.TaskID, "PRReviewsCollected", "", t.TaskID, map[string]any{"target_id": t.ID, "head_sha": b.head}); err != nil {
					return err
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE source_review SET feedback_sent=1 WHERE target_id=? AND head_sha=?`, b.target, b.head); err != nil {
				return err
			}
		}
		return nil
	})
}
