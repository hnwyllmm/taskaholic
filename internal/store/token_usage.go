package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"work-assistant/internal/model"
)

func migrateV21(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='run_token_usage'`).Scan(&exists); err != nil {
		return err
	}
	if version >= 21 && exists != 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS run_token_usage(
		runtime_id TEXT NOT NULL,
		epoch TEXT NOT NULL,
		runtime_seq INTEGER NOT NULL,
		run_id TEXT NOT NULL REFERENCES run(run_id),
		provider TEXT NOT NULL,
		input_tokens INTEGER NOT NULL,
		cached_input_tokens INTEGER NOT NULL,
		cache_write_input_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		reasoning_output_tokens INTEGER NOT NULL,
		recorded_at_ms INTEGER NOT NULL,
		PRIMARY KEY(runtime_id,epoch,runtime_seq)
	)`); err != nil {
		return err
	}
	if _, err = tx.Exec(`CREATE INDEX IF NOT EXISTS run_token_usage_run_idx ON run_token_usage(run_id)`); err != nil {
		return err
	}

	// Preserve already observed Codex usage. Before v21 it was stored as a
	// normal RunProgress event with stream=codex-usage.
	rows, err := tx.Query(`SELECT payload_json FROM event_log WHERE event_type='RunProgress'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var event model.RuntimeEvent
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		usage := event.Usage
		if usage == nil && event.Stream == "codex-usage" {
			var legacy model.TokenUsage
			if json.Unmarshal([]byte(event.Message), &legacy) == nil {
				legacy.Provider = "codex"
				usage = &legacy
			}
		}
		if usage != nil {
			event.Usage = usage
			if err = recordTokenUsageTx(context.Background(), tx, event, event.OccurredAt); err != nil {
				rows.Close()
				return fmt.Errorf("backfill token usage: %w", err)
			}
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO schema_version VALUES(21,unixepoch('subsec')*1000)`); err != nil {
		return err
	}
	return tx.Commit()
}

func recordTokenUsageTx(ctx context.Context, tx *sql.Tx, event model.RuntimeEvent, now int64) error {
	u := event.Usage
	if u == nil {
		return nil
	}
	if event.RuntimeID == "" || event.Epoch == "" || event.RuntimeSeq <= 0 || event.RunID == "" {
		return errors.New("token usage requires runtime event identity")
	}
	if u.InputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteInputTokens < 0 || u.OutputTokens < 0 || u.ReasoningOutputTokens < 0 || u.CachedInputTokens > u.InputTokens || u.ReasoningOutputTokens > u.OutputTokens {
		return errors.New("invalid token usage")
	}
	provider := u.Provider
	if provider == "" {
		provider = "unknown"
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO run_token_usage(
		runtime_id,epoch,runtime_seq,run_id,provider,input_tokens,cached_input_tokens,
		cache_write_input_tokens,output_tokens,reasoning_output_tokens,recorded_at_ms
	) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, event.RuntimeID, event.Epoch, event.RuntimeSeq,
		event.RunID, provider, u.InputTokens, u.CachedInputTokens, u.CacheWriteInputTokens,
		u.OutputTokens, u.ReasoningOutputTokens, now)
	return err
}

var tokenStageLabels = map[string]string{
	"planning":     "方案设计",
	"development":  "开发实现",
	"review":       "Agent 评审",
	"acceptance":   "验收沟通",
	"testing":      "测试验证",
	"consultation": "旁路问答",
	"other":        "其他",
}

func addTokenTotals(dst *model.TokenUsageTotals, src model.TokenUsageTotals) {
	dst.InputTokens += src.InputTokens
	dst.CachedInputTokens += src.CachedInputTokens
	dst.CacheWriteInputTokens += src.CacheWriteInputTokens
	dst.OutputTokens += src.OutputTokens
	dst.ReasoningOutputTokens += src.ReasoningOutputTokens
	dst.TotalTokens += src.TotalTokens
}

// TaskTokenUsage includes the selected task and every execution/review child.
// It reports coverage separately because adapters and historical runs may not
// expose usage; an unavailable measurement must never look like zero cost.
func (s *Store) TaskTokenUsage(ctx context.Context, taskID string) (model.TaskTokenUsage, error) {
	var result model.TaskTokenUsage
	const scope = `WITH RECURSIVE scope(task_id) AS (
		SELECT ? UNION SELECT e.to_task_id FROM task_edge e JOIN scope s ON e.from_task_id=s.task_id
		WHERE e.edge_type IN ('DECOMPOSED_INTO','REVIEWS')
	)`
	if err := s.db.QueryRowContext(ctx, scope+`
		SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN r.state IN ('QUEUED','RUNNING') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.state NOT IN ('QUEUED','RUNNING') AND u.run_id IS NULL THEN 1 ELSE 0 END),0)
		FROM run r JOIN scope s ON s.task_id=r.task_id
		LEFT JOIN (SELECT DISTINCT run_id FROM run_token_usage) u ON u.run_id=r.run_id`, taskID).
		Scan(&result.RunCount, &result.PendingRuns, &result.UnreportedRuns); err != nil {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, scope+`
		SELECT r.run_id,r.task_id,t.title,
		CASE
		 WHEN EXISTS(SELECT 1 FROM review_turn q WHERE q.run_id=r.run_id) THEN 'acceptance'
		 WHEN EXISTS(SELECT 1 FROM environment_job j WHERE j.task_id=r.task_id) THEN 'testing'
		 WHEN EXISTS(SELECT 1 FROM task_consultation c WHERE c.execution_task_id=r.task_id) THEN 'consultation'
		 WHEN EXISTS(SELECT 1 FROM task_edge e WHERE e.to_task_id=r.task_id AND e.edge_type='REVIEWS') THEN 'review'
		 WHEN EXISTS(SELECT 1 FROM development_run d WHERE d.run_id=r.run_id AND d.phase='PLANNING') THEN 'planning'
		 WHEN EXISTS(SELECT 1 FROM development_run d WHERE d.run_id=r.run_id AND d.phase='AGENT_REVIEW') THEN 'review'
		 WHEN EXISTS(SELECT 1 FROM development_run d WHERE d.run_id=r.run_id AND d.phase='IMPLEMENTING') THEN 'development'
		 ELSE 'other' END stage,
		GROUP_CONCAT(DISTINCT u.provider),COALESCE(r.model_id,''),COUNT(*),
		SUM(u.input_tokens),SUM(u.cached_input_tokens),SUM(u.cache_write_input_tokens),
		SUM(u.output_tokens),SUM(u.reasoning_output_tokens),SUM(u.input_tokens+u.output_tokens)
		FROM run_token_usage u JOIN run r ON r.run_id=u.run_id JOIN task t ON t.task_id=r.task_id
		JOIN scope s ON s.task_id=r.task_id GROUP BY r.run_id ORDER BY r.created_at_ms`, taskID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	stageIndex := map[string]int{}
	for rows.Next() {
		var run model.RunTokenUsage
		if err = rows.Scan(&run.RunID, &run.TaskID, &run.TaskTitle, &run.Stage, &run.Provider, &run.ModelID, &run.Turns,
			&run.InputTokens, &run.CachedInputTokens, &run.CacheWriteInputTokens, &run.OutputTokens,
			&run.ReasoningOutputTokens, &run.TotalTokens); err != nil {
			return result, err
		}
		run.StageLabel = tokenStageLabels[run.Stage]
		idx, ok := stageIndex[run.Stage]
		if !ok {
			idx = len(result.Stages)
			stageIndex[run.Stage] = idx
			result.Stages = append(result.Stages, model.StageTokenUsage{Stage: run.Stage, Label: run.StageLabel, Runs: []model.RunTokenUsage{}})
		}
		result.Stages[idx].Runs = append(result.Stages[idx].Runs, run)
		addTokenTotals(&result.Stages[idx].TokenUsageTotals, run.TokenUsageTotals)
		addTokenTotals(&result.TokenUsageTotals, run.TokenUsageTotals)
		result.ReportedRuns++
	}
	return result, rows.Err()
}
