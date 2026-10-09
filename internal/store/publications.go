package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func migrateV16(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version >= 16 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE publication(publication_key TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES task(task_id), state TEXT NOT NULL, next_attempt_ms INTEGER NOT NULL, data_json TEXT NOT NULL)`,
		`CREATE INDEX publication_due ON publication(state,next_attempt_ms)`,
		`CREATE TABLE publication_history(seq INTEGER PRIMARY KEY AUTOINCREMENT, publication_key TEXT NOT NULL REFERENCES publication(publication_key), version INTEGER NOT NULL, created_at_ms INTEGER NOT NULL, data_json TEXT NOT NULL)`,
		`INSERT INTO schema_version VALUES(16,unixepoch('subsec')*1000)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return fmt.Errorf("migrate v16: %w", err)
		}
	}
	return tx.Commit()
}

func publicationKey(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
}

func githubReviewDiscipline(role model.Role) string {
	has := func(want ...string) bool {
		for _, capability := range role.Capabilities {
			for _, candidate := range want {
				if capability == candidate {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("architecture.review"):
		return "Architecture reviewer"
	case has("qa.review", "test.review"):
		return "QA / test reviewer"
	case has("security.review"):
		return "Security reviewer"
	case has("code.review"):
		return "General code reviewer"
	case has("product.review"):
		return "Product reviewer"
	case has("performance.review"):
		return "Performance reviewer"
	case has("design.review"):
		return "Design reviewer"
	case has("delivery.review"):
		return "Delivery reviewer"
	default:
		return "Independent reviewer"
	}
}

// Reviewer and member names are administrator-configured display values. Keep
// them readable in GitHub while preventing a name from injecting new Markdown
// structure, links or mentions into an externally written review comment.
func githubReviewIdentityText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "Not assigned yet"
	}
	return strings.NewReplacer(
		`\`, `\\`, "`", `\`+"`", "*", `\*`, "_", `\_`,
		"[", `\[`, "]", `\]`, "<", "&lt;", ">", "&gt;",
		"#", `\#`, "|", `\|`, "@", "&#64;",
	).Replace(value)
}

func savePublicationTx(ctx context.Context, tx *sql.Tx, p model.Publication) error {
	p.UpdatedAtMS = time.Now().UnixMilli()
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO publication VALUES(?,?,?,?,?) ON CONFLICT(publication_key) DO UPDATE SET task_id=excluded.task_id,state=excluded.state,next_attempt_ms=excluded.next_attempt_ms,data_json=excluded.data_json`, p.Key, p.TaskID, p.State, p.NextAttemptMS, raw); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task SET version=version+1,updated_at_ms=? WHERE task_id=? OR task_id IN (SELECT t.task_id FROM source_review r JOIN source_target t USING(target_id) WHERE r.task_id=?)`, p.UpdatedAtMS, p.TaskID, p.TaskID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO publication_history(publication_key,version,created_at_ms,data_json) VALUES(?,?,?,?)`, p.Key, p.Version, p.UpdatedAtMS, raw)
	return err
}
func queuePublicationTx(ctx context.Context, tx *sql.Tx, p model.Publication) error {
	old, err := readJSONRow[model.Publication](tx.QueryRowContext(ctx, `SELECT data_json FROM publication WHERE publication_key=?`, p.Key))
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	// Never lose the exact attempted body needed to reconcile an ambiguous POST.
	if old.State == "SUBMITTING" || old.State == "UNCERTAIN" || old.State == "BLOCKED" {
		return nil
	}
	if old.Body == p.Body && old.HeadSHA == p.HeadSHA && old.ReportHash == p.ReportHash && old.Verdict == p.Verdict && old.DesiredStatus == p.DesiredStatus {
		return nil
	}
	p.Version = old.Version + 1
	p.AppliedVersion = old.AppliedVersion
	p.RemoteID = old.RemoteID
	p.RemoteURL = old.RemoteURL
	p.AppliedBody = old.AppliedBody
	p.State = "QUEUED"
	return savePublicationTx(ctx, tx, p)
}

// Reconcile derives destinations from durable bindings, never URLs in model prose.
// Pollers remain read-only. History is append-only and included in SQLite backups.
func (s *Store) ReconcilePublications(ctx context.Context) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT r.task_id,r.role_id,r.head_sha,r.state,t.data_json,role.data_json,COALESCE((SELECT output FROM run WHERE task_id=r.task_id AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1),''),COALESCE(review_task.assigned_agent_id,''),COALESCE(json_extract(agent.data_json,'$.name'),'') FROM source_review r JOIN source_target t USING(target_id) JOIN role USING(role_id) JOIN task review_task ON review_task.task_id=r.task_id LEFT JOIN agent_profile agent ON agent.agent_id=review_task.assigned_agent_id WHERE r.state!='SUPERSEDED' AND r.head_sha=json_extract(t.data_json,'$.head_sha') ORDER BY r.rowid`)
		if err != nil {
			return err
		}
		type review struct {
			task, role, head, state, output, agent, agentName string
			target                                            model.SourceTarget
			roleSnapshot                                      model.Role
		}
		var reviews []review
		for rows.Next() {
			var r review
			var targetRaw, roleRaw []byte
			if err = rows.Scan(&r.task, &r.role, &r.head, &r.state, &targetRaw, &roleRaw, &r.output, &r.agent, &r.agentName); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal(targetRaw, &r.target); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal(roleRaw, &r.roleSnapshot); err != nil {
				rows.Close()
				return err
			}
			reviews = append(reviews, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range reviews {
			key := publicationKey("pr-review", r.target.Entity, r.role)
			verdict := "Review in progress (no conclusion yet)"
			message := ""
			var result workflow.Result
			if r.output != "" {
				result, err = workflow.Parse(r.output)
				if err != nil {
					result = workflow.Result{Message: "The review report is invalid and cannot be treated as approval."}
				}
				message = result.Message
				switch result.ReviewDecision {
				case "passed":
					verdict = "Passed"
				case "changes_requested":
					verdict = "Changes requested"
				default:
					verdict = "Not passed: the review is incomplete or the previous report has no explicit verdict"
				}
			}
			var active bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('RUNNING','QUEUED')) OR EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING')`, r.task, r.task).Scan(&active); err != nil {
				return err
			}
			if active {
				verdict = "Re-review in progress; the previous verdict does not apply to this revision"
			}
			if r.state != "COMPLETED" && result.ReviewDecision == "passed" {
				verdict = "Not passed: this review run has not completed"
			}
			w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, r.task))
			if err != nil {
				return err
			}
			if w.Paused {
				verdict = "Paused; this is not a current passing verdict"
			}
			discipline := githubReviewDiscipline(r.roleSnapshot)
			member := fmt.Sprintf("**%s**", githubReviewIdentityText(r.agentName))
			if r.agent != "" {
				member += fmt.Sprintf(" (`%s`)", r.agent)
			}
			body := fmt.Sprintf("<!-- work-assistant:review:%s -->\n## Agent code review · %s\n\nReviewer role: **%s** (`%s`)\nReviewer member: %s\nReview task: `%s`\n\nReviewed commit: `%s`\n\nVerdict: **%s**\n\n%s\n\n---\nThis verdict records the role-specific code review only. CI and requested test pipelines are tracked as separate delivery gates by the original task. This is not a GitHub approval, human acceptance, or merge authorization. Every new commit requires another review. This comment is updated in place; previous versions remain in the Work Assistant history.", key, discipline, githubReviewIdentityText(r.roleSnapshot.Name), r.role, member, r.task, r.head, verdict, message)
			decision := "pending"
			if verdict == "Passed" {
				decision = "passed"
			}
			if err = queuePublicationTx(ctx, tx, model.Publication{Key: key, TaskID: r.task, Platform: "github", URL: r.target.Entity, HeadSHA: r.head, Body: body, Sticky: true, ReportHash: publicationKey(r.output), Verdict: decision}); err != nil {
				return err
			}
		}
		return reconcileIssuePublicationsTx(ctx, tx)
	})
}

func reconcileIssuePublicationsTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT se.task_id,se.entity,s.data_json FROM source_entity se JOIN task_source s USING(source_id) WHERE json_extract(s.data_json,'$.kind') IN ('antmultica','github') ORDER BY se.rowid`)
	if err != nil {
		return err
	}
	type binding struct {
		task, entity string
		source       model.TaskSource
	}
	var bindings []binding
	for rows.Next() {
		var b binding
		var raw []byte
		if err = rows.Scan(&b.task, &b.entity, &raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &b.source); err != nil {
			rows.Close()
			return err
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range bindings {
		p := model.Publication{TaskID: b.task, Platform: b.source.Kind}
		if b.source.Kind == "antmultica" {
			parts := strings.Split(b.entity, ":")
			if len(parts) != 3 || parts[0] != "antmultica" || parts[1] != b.source.Config.WorkspaceID {
				continue
			}
			p.WorkspaceID = parts[1]
			p.IssueID = parts[2]
			p.ActorID = b.source.Config.AssigneeID
		} else {
			_, _, _, url, err := model.ParseGitHubIssue(b.entity)
			if err != nil {
				continue
			}
			p.URL = url
		}
		refs, _, err := taskReferences(ctx, tx, b.task)
		if err != nil {
			return err
		}
		destination := ""
		var links strings.Builder
		for _, ref := range refs {
			if ref.Kind == "antmultica.issue" {
				destination = ref.URL
			}
			if ref.Kind == "github.pr" {
				if p.Platform == "github" {
					fmt.Fprintf(&links, "\n- PR: %s", ref.URL)
				} else {
					fmt.Fprintf(&links, "\n- PR：%s", ref.URL)
				}
			}
		}
		if p.URL == "" {
			p.URL = destination
		}
		if p.URL == "" {
			continue
		}
		task, err := getTaskTx(ctx, tx, b.task)
		if err != nil {
			return err
		}
		if p.Platform == "antmultica" && task.State != model.TaskStateCompleted {
			// A source only observes and binds work. Once the assigned business
			// Agent has actually started a Run, Manager owns the lifecycle
			// writeback. Deriving this from durable started_at_ms also reconciles
			// tasks that were already running when the service was upgraded.
			var started bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE task_id=? AND started_at_ms IS NOT NULL)`, b.task).Scan(&started); err != nil {
				return err
			}
			if started {
				status := p
				status.Key = publicationKey("issue-status", b.entity, b.task, "in_progress")
				status.DesiredStatus = "in_progress"
				if err = queuePublicationTx(ctx, tx, status); err != nil {
					return err
				}
			}
		}
		d, developmentErr := developmentTx(ctx, tx, b.task)
		if developmentErr != nil && developmentErr != sql.ErrNoRows {
			return developmentErr
		}
		if developmentErr == nil && (d.Phase != "IMPLEMENTING" || d.ApprovedReviewID == "") {
			// Drafts and review discussions stay inside the task. Agent approval
			// alone is not final approval; do not publish while waiting for a human.
			continue
		}
		if developmentErr == nil {
			// Publish the approved version even if implementation completed before
			// the next reconciliation tick. Never substitute its latest output.
			if err = queueApprovedIssuePlanTx(ctx, tx, p, b.entity, d); err != nil {
				return err
			}
		}
		var runID, output string
		err = tx.QueryRowContext(ctx, `SELECT run_id,output FROM run WHERE task_id=? AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1`, b.task).Scan(&runID, &output)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if runID == "" {
			continue
		} // Do not copy historical descriptions/credentials into comments.
		if developmentErr == nil && runID == d.PlanRunID {
			continue // The immutable approved-plan milestone was handled above.
		}
		result, err := workflow.Parse(output)
		if err != nil {
			continue
		}
		if result.TaskUpdate == nil {
			continue
		} // Old free-form reports were not authored for external publication.
		u := result.TaskUpdate
		pipelines, err := listJSONRows[model.TestPipeline](ctx, tx, `SELECT data_json FROM test_pipeline WHERE task_id=? ORDER BY rowid`, b.task)
		if err != nil {
			return err
		}
		for _, pipeline := range pipelines {
			if p.Platform == "github" {
				fmt.Fprintf(&links, "\n- Pipeline #%d: %s · %s · tested commit %s", pipeline.PipelineID, pipeline.State, pipeline.URL, pipeline.HeadSHA)
			} else {
				fmt.Fprintf(&links, "\n- Pipeline #%d：%s · %s · 被测 commit %s", pipeline.PipelineID, pipeline.State, pipeline.URL, pipeline.HeadSHA)
			}
		}
		lifecycle := ""
		if developmentErr == nil {
			if p.Platform == "github" {
				lifecycle = "\nDevelopment phase: " + d.Phase + " (implementation starts only after plan approval; plan approval does not complete the issue)."
			} else {
				lifecycle = "\n开发阶段：" + d.Phase + "（方案批准前不开发；方案通过不代表工单完成）"
			}
		}
		// External issues are milestone reports, not a mirror of the execution
		// log. Freeze each delivery once; retries, new runs, CI updates and
		// temporary blockers must not create or rewrite issue comments.
		var milestones []string
		for _, ref := range refs {
			if ref.Kind == "github.pr" {
				milestones = append(milestones, "pr:"+ref.URL)
			}
		}
		if task.State == "COMPLETED" {
			milestones = append(milestones, "completed")
		}
		for _, milestone := range milestones {
			key := publicationKey("issue-milestone", b.entity, b.task, milestone)
			legacyEvidence := "状态：COMPLETED\n"
			if strings.HasPrefix(milestone, "pr:") {
				legacyEvidence = "- PR：" + strings.TrimPrefix(milestone, "pr:") + "\n"
			}
			var exists bool
			// Recognize pre-upgrade reports too, without editing their history or
			// sending a duplicate delivery after upgrade.
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publication WHERE publication_key=? OR (task_id=? AND json_extract(data_json,'$.url')=? AND instr(json_extract(data_json,'$.body'),'工作助手处理进展')>0 AND instr(json_extract(data_json,'$.body'),?)>0))`, key, b.task, p.URL, legacyEvidence).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			body := ""
			if p.Platform == "github" {
				body = fmt.Sprintf("<!-- work-assistant:progress:%s -->\n## Work Assistant progress\n\nStatus: %s\nTask type: %s\n\nThis report contains the latest submitted analysis. Ongoing changes made after that submission are not included.\n\nProblem analysis: %s\n\nImplementation/fix approach: %s\n\nRationale: %s\n\nValidation: %s\n\nBlocked/unresolved reason: %s\n\nRelated delivery:%s\n\nThis status comes from Work Assistant records. Opening a PR does not mean it has been merged or that the issue is resolved.", key, task.State, u.Kind, u.Analysis, u.Approach, u.Reason, u.Validation, u.BlockedReason, links.String())
			} else {
				body = fmt.Sprintf("<!-- work-assistant:progress:%s -->\n工作助手处理进展\n\n状态：%s\n任务类型：%s\n\n以下是最近一轮已提交的分析，正在进行的后续修改尚不包含在此报告内。\n\n问题分析：%s\n\n实现/修复方案：%s\n\n修改理由：%s\n\n验证结果：%s\n\n阻塞/无法修复说明：%s\n\n关联交付：%s\n\n状态为工作助手记录；提交 PR 不代表已合并或工单已解决。", key, task.State, u.Kind, u.Analysis, u.Approach, u.Reason, u.Validation, u.BlockedReason, links.String())
			}
			p.Key = key
			body += lifecycle
			p.Body = body
			if err = queuePublicationTx(ctx, tx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func queueApprovedIssuePlanTx(ctx context.Context, tx *sql.Tx, p model.Publication, entity string, d model.Development) error {
	if !d.PublishApprovedPlan {
		return nil // Historical approvals are deliberately not backfilled.
	}
	// One immutable milestone per approval, independent of task status or links.
	p.Key = publicationKey("issue-approved-plan", entity, p.TaskID, d.ApprovedReviewID, d.PlanHash)
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publication WHERE publication_key=?)`, p.Key).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	var output string
	if err := tx.QueryRowContext(ctx, `SELECT output FROM run WHERE task_id=? AND run_id=? AND state='COMPLETED'`, p.TaskID, d.PlanRunID).Scan(&output); err != nil {
		return err
	}
	result, err := workflow.Parse(output)
	if err != nil || result.TaskUpdate == nil {
		return nil // Do not publish internal prose that was not authored for the source.
	}
	u := result.TaskUpdate
	if p.Platform == "github" {
		p.Body = fmt.Sprintf("<!-- work-assistant:progress:%s -->\n## Final approved plan\n\nThe plan passed Agent review and human confirmation. This does not mean implementation, testing, or the issue itself is complete.\n\nRepository: %s\nBase branch: %s\nPlan version: %d\n\nProblem analysis: %s\n\nImplementation/fix approach: %s\n\nRationale: %s\n\nExisting validation: %s\n\nRemaining validation/limitations: %s", p.Key, d.Repository, d.BaseBranch, d.Version, u.Analysis, u.Approach, u.Reason, u.Validation, u.BlockedReason)
	} else {
		p.Body = fmt.Sprintf("<!-- work-assistant:progress:%s -->\n最终确认的方案\n\n方案已通过 Agent 评审及人工确认；不代表实现、测试或工单已完成。\n\n仓库：%s\n目标分支：%s\n方案版本：%d\n\n问题分析：%s\n\n实现/修复方案：%s\n\n修改理由：%s\n\n已有验证：%s\n\n待验证/限制：%s", p.Key, d.Repository, d.BaseBranch, d.Version, u.Analysis, u.Approach, u.Reason, u.Validation, u.BlockedReason)
	}
	return queuePublicationTx(ctx, tx, p)
}

func (s *Store) ListPublications(ctx context.Context, taskID string) ([]model.Publication, error) {
	if taskID == "" {
		return listJSONRows[model.Publication](ctx, s.db, `SELECT data_json FROM publication ORDER BY rowid`)
	}
	return listJSONRows[model.Publication](ctx, s.db, `SELECT data_json FROM publication WHERE task_id=? OR task_id IN (SELECT r.task_id FROM source_review r JOIN source_target t USING(target_id) WHERE t.task_id=?) ORDER BY rowid`, taskID, taskID)
}
func (s *Store) ClaimPublication(ctx context.Context, key string) (model.Publication, error) {
	var p model.Publication
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		var err error
		p, err = readJSONRow[model.Publication](tx.QueryRowContext(ctx, `SELECT data_json FROM publication WHERE publication_key=?`, key))
		if err != nil {
			return err
		}
		if p.State == "SYNCED" || p.State == "BLOCKED" || p.State == "SKIPPED" || p.NextAttemptMS > time.Now().UnixMilli() {
			return model.ErrConflict
		}
		// SUBMITTING surviving a process crash is ambiguous; never POST again.
		reconcile := p.State == "SUBMITTING" || p.State == "UNCERTAIN"
		p.State = "SUBMITTING"
		p.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		if err = savePublicationTx(ctx, tx, p); err != nil {
			return err
		}
		if reconcile {
			p.State = "UNCERTAIN"
		}
		return nil
	})
	return p, err
}
func (s *Store) FinishPublication(ctx context.Context, p model.Publication, remoteID, remoteURL, state, message string) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		current, err := readJSONRow[model.Publication](tx.QueryRowContext(ctx, `SELECT data_json FROM publication WHERE publication_key=?`, p.Key))
		if err != nil {
			return err
		}
		if current.Version != p.Version {
			return model.ErrConflict
		}
		current.State = state
		current.Error = message
		current.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		if remoteID != "" {
			current.RemoteID = remoteID
			current.RemoteURL = remoteURL
		}
		if state == "SYNCED" {
			current.AppliedVersion = p.Version
			current.AppliedBody = p.Body
			current.Error = ""
		}
		return savePublicationTx(ctx, tx, current)
	})
}
