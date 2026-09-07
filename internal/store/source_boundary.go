package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Remove execution settings from source configuration, preserving the exact
// previous JSON in the event log. Task requirements, ownership, native Sessions,
// historical review batches and polling cursors are deliberately untouched.
func migrateV14(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version >= 14 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT source_id,data_json FROM task_source WHERE
		json_type(data_json,'$.config.role_id') IS NOT NULL OR
		json_type(data_json,'$.config.reviewer_role_ids') IS NOT NULL OR
		json_type(data_json,'$.config.project_id') IS NOT NULL OR
		json_type(data_json,'$.config.defer_assignment') IS NOT NULL`)
	if err != nil {
		return err
	}
	type legacy struct{ id, data string }
	var sources []legacy
	for rows.Next() {
		var source legacy
		if err = rows.Scan(&source.id, &source.data); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, source := range sources {
		if _, err = appendEventTx(context.Background(), tx, "source", source.id, "SourceExecutionSettingsRetired", "", source.id, map[string]any{
			"previous_source": json.RawMessage(source.data),
			"reason":          "任务源仅采集事件。新任务交给 Router；已有任务和 Session 不重新分配。PR 评审改由 Router 按仓库及评审能力规划。",
		}); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE task_source SET data_json=json_set(
			json_remove(data_json,'$.config.role_id','$.config.reviewer_role_ids','$.config.project_id','$.config.defer_assignment'),
			'$.version',json_extract(data_json,'$.version')+1) WHERE source_id=?`, source.id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO schema_version VALUES(14,unixepoch('subsec')*1000)`); err != nil {
		return err
	}
	return tx.Commit()
}
