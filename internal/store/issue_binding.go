package store

import (
	"context"
	"database/sql"
	"fmt"
	"work-assistant/internal/model"
)

// Explicit user/API binding until a GitHub Issue ingestion policy is configured.
// Never infer original issues from PR bodies or other untrusted prose.
func (s *Store) BindGitHubIssue(ctx context.Context, taskID, sourceID, url string) error {
	_, _, _, canonical, err := model.ParseGitHubIssue(url)
	if err != nil {
		return err
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		if _, err := getTaskTx(ctx, tx, taskID); err != nil {
			return err
		}
		source, err := readJSONRow[model.TaskSource](tx.QueryRowContext(ctx, `SELECT data_json FROM task_source WHERE source_id=?`, sourceID))
		if err != nil {
			return err
		}
		if source.Kind != "github" {
			return fmt.Errorf("%w: must select GitHub source", model.ErrValidation)
		}
		var oldTask string
		err = tx.QueryRowContext(ctx, `SELECT task_id FROM source_entity WHERE entity=?`, canonical).Scan(&oldTask)
		if err == nil {
			if oldTask == taskID {
				return nil
			}
			return fmt.Errorf("%w: issue already belongs to another task", model.ErrConflict)
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO source_entity(source_id,entity,task_id) VALUES(?,?,?)`, sourceID, canonical, taskID); err != nil {
			return err
		}
		_, err = insertMessageTx(ctx, tx, taskID, "system", "已关联原 GitHub Issue，后续结构化进展将回写此页面："+canonical, "", "RECORDED")
		return err
	})
}
