package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"work-assistant/internal/model"
	"work-assistant/internal/router"
)

func migrateV12(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 12 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Historical runs remain unknown, never backfilled with today's defaults.
	if _, err = tx.Exec(`ALTER TABLE run ADD COLUMN execution_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO schema_version VALUES(12,unixepoch('subsec')*1000)`); err != nil {
		return err
	}
	return tx.Commit()
}

func validateRuntimeEffortTx(ctx context.Context, tx *sql.Tx, runtimeID, adapterID, effort string) error {
	if err := model.ValidateReasoningEffort(effort); err != nil {
		return err
	}
	if effort == "" {
		return nil
	}
	var caps []byte
	if err := tx.QueryRowContext(ctx, `SELECT capabilities_json FROM runtime WHERE runtime_id=?`, runtimeID).Scan(&caps); err != nil {
		return err
	}
	if !router.SupportsFeature(model.Runtime{Capabilities: caps}, adapterID, "reasoning_effort") {
		return fmt.Errorf("%w: 此机器的 Agent 暂不支持独立推理强度", model.ErrValidation)
	}
	return nil
}

// Effort is a per-run setting, not session identity. A member edit affects the
// next Run on the same model, never a queued/running snapshot. When the member
// switches model, old sessions retain their own initial model/effort pair.
func resolveExecutionTx(ctx context.Context, tx *sql.Tx, session model.Session, override *string) (model.ExecutionSettings, error) {
	settings := model.ExecutionSettings{ReasoningEffortSource: "runtime_default"}
	if override != nil {
		settings.ReasoningEffort, settings.ReasoningEffortSource = *override, "run_override"
	} else {
		a, err := scanAgent(tx.QueryRowContext(ctx, agentSelect+` WHERE a.agent_id=?`, session.AgentID))
		if err != nil && err != sql.ErrNoRows {
			return settings, err
		}
		if err == nil && a.ModelID == session.ModelID {
			settings.ReasoningEffort = a.ReasoningEffort
			if a.ReasoningEffort != "" {
				settings.ReasoningEffortSource = "member_default"
			}
		} else {
			var meta struct {
				Defaults *model.ExecutionSettings `json:"execution_defaults"`
			}
			if err := json.Unmarshal(session.Metadata, &meta); err != nil {
				return settings, err
			}
			if meta.Defaults != nil {
				settings.ReasoningEffort = meta.Defaults.ReasoningEffort
				settings.ReasoningEffortSource = "session_default"
			}
		}
	}
	return settings, validateRuntimeEffortTx(ctx, tx, session.RuntimeID, session.AdapterID, settings.ReasoningEffort)
}

func applyExecutionTx(ctx context.Context, tx *sql.Tx, event model.RuntimeEvent) error {
	run, err := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE run_id=?`, event.RunID))
	if err != nil {
		return err
	}
	if run.State != model.RunStateQueued && run.State != model.RunStateRunning {
		return nil
	}
	config := event.Execution
	if config == nil || !config.ExecutionConfigured || config.ReasoningEffort != run.ReasoningEffort || len(config.ExecutionModelID) > 200 || strings.ContainsAny(config.ExecutionModelID, "\r\n\x00\x1b") {
		return fmt.Errorf("%w: execution configuration does not match the queued run", model.ErrValidation)
	}
	if run.ExecutionConfigured && run.ExecutionModelID != config.ExecutionModelID {
		return fmt.Errorf("%w: execution configuration already recorded", model.ErrConflict)
	}
	run.ExecutionModelID, run.ExecutionConfigured = config.ExecutionModelID, true
	data, err := json.Marshal(run.ExecutionSettings)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE run SET execution_json=? WHERE run_id=?`, data, run.ID)
	return err
}
