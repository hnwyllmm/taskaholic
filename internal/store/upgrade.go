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
)

func migrateV7(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version=7`).Scan(&n); err != nil || n > 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE upgrade_job(upgrade_id TEXT PRIMARY KEY,state TEXT NOT NULL,data_json TEXT NOT NULL)`,
		`CREATE TABLE maintenance(singleton INTEGER PRIMARY KEY CHECK(singleton=1),upgrade_id TEXT NOT NULL)`,
		`INSERT INTO maintenance VALUES(1,'')`,
		`INSERT INTO schema_version VALUES(7,unixepoch('subsec')*1000)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func createUpgradeTx(ctx context.Context, tx *sql.Tx, chatID, title, instructions string) (model.Upgrade, error) {
	var u model.Upgrade
	if strings.TrimSpace(title) == "" || len(title) > 400 || strings.TrimSpace(instructions) == "" || len(instructions) > 32000 {
		return u, fmt.Errorf("%w: invalid upgrade request", model.ErrValidation)
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM upgrade_job WHERE state IN ('QUEUED','BUILDING','READY','WAITING_IDLE','INSTALLING')`).Scan(&active); err != nil {
		return u, err
	}
	if active > 0 {
		return u, fmt.Errorf("%w: 已有升级在准备或待确认，请先处理或取消它", model.ErrConflict)
	}
	now := time.Now().UnixMilli()
	u = model.Upgrade{ID: id.New("upgrade"), ChatID: chatID, Title: title, Instructions: instructions, State: "QUEUED", Version: 1, Changes: []model.UpgradeChange{}, CreatedAtMS: now, UpdatedAtMS: now}
	binding, err := getSystemBindingTx(ctx, tx, "upgrade_builder")
	if err != nil {
		return u, err
	}
	u.BuilderBinding = &binding
	if binding.Mode == "agent" {
		a, e := systemAgentTx(ctx, tx, binding.AgentID, binding.Slot)
		if e != nil {
			return u, e
		}
		if a.ActiveRuns >= a.MaxConcurrent {
			return u, fmt.Errorf("%w: 升级构建成员正在忙，请稍后重试", model.ErrConflict)
		}
		u.Builder = &a
	}
	raw, _ := json.Marshal(u)
	if _, err := tx.ExecContext(ctx, `INSERT INTO upgrade_job VALUES(?,?,?)`, u.ID, u.State, raw); err != nil {
		return u, err
	}
	_, err = appendEventTx(ctx, tx, "upgrade", u.ID, "UpgradeRequested", "", u.ID, map[string]any{"title": title, "chat_id": chatID, "builder": u.Builder, "binding": binding})
	return u, err
}

func (s *Store) CreateUpgrade(ctx context.Context, title, instructions string) (model.Upgrade, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Upgrade{}, err
	}
	defer tx.Rollback()
	u, err := createUpgradeTx(ctx, tx, "", title, instructions)
	if err != nil {
		return u, err
	}
	return u, tx.Commit()
}
func (s *Store) ListUpgrades(ctx context.Context) ([]model.Upgrade, error) {
	return listJSONRows[model.Upgrade](ctx, s.db, `SELECT data_json FROM upgrade_job ORDER BY rowid DESC LIMIT 20`)
}
func (s *Store) GetUpgrade(ctx context.Context, upgradeID string) (model.Upgrade, error) {
	return readJSONRow[model.Upgrade](s.db.QueryRowContext(ctx, `SELECT data_json FROM upgrade_job WHERE upgrade_id=?`, upgradeID))
}
func getUpgradeTx(ctx context.Context, tx *sql.Tx, upgradeID string) (model.Upgrade, error) {
	return readJSONRow[model.Upgrade](tx.QueryRowContext(ctx, `SELECT data_json FROM upgrade_job WHERE upgrade_id=?`, upgradeID))
}
func saveUpgradeTx(ctx context.Context, tx *sql.Tx, u *model.Upgrade, event string) error {
	u.Version++
	u.UpdatedAtMS = time.Now().UnixMilli()
	if runes := []rune(u.Log); len(runes) > 60000 {
		u.Log = "…\n" + string(runes[len(runes)-15000:])
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE upgrade_job SET state=?,data_json=? WHERE upgrade_id=?`, u.State, raw, u.ID); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "upgrade", u.ID, event, "", u.ID, map[string]any{"state": u.State, "version": u.Version, "candidate_sha256": u.CandidateSHA256, "error": u.Error})
	return err
}

// ChangeUpgrade provides CAS for the local worker, including cancellation races.
func (s *Store) ChangeUpgrade(ctx context.Context, u model.Upgrade, expected int64, event string) (model.Upgrade, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return u, err
	}
	defer tx.Rollback()
	current, err := getUpgradeTx(ctx, tx, u.ID)
	if err != nil {
		return u, err
	}
	if current.Version != expected {
		return u, fmt.Errorf("%w: upgrade changed", model.ErrConflict)
	}
	u.Builder, u.BuilderBinding = current.Builder, current.BuilderBinding
	if current.State == "QUEUED" && u.State == "BUILDING" && current.Builder != nil {
		a, e := systemAgentTx(ctx, tx, current.Builder.ID, "upgrade_builder")
		if e != nil {
			return u, e
		}
		if a.ActiveRuns > a.MaxConcurrent {
			return u, fmt.Errorf("%w: 升级构建成员没有可用并发容量", model.ErrConflict)
		}
	}
	u.Version = current.Version
	if err = saveUpgradeTx(ctx, tx, &u, event); err != nil {
		return u, err
	}
	return u, tx.Commit()
}
func (s *Store) Maintenance(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT upgrade_id FROM maintenance WHERE singleton=1`).Scan(&v)
	return v, err
}
func checkMaintenanceTx(ctx context.Context, tx *sql.Tx) error {
	var v string
	if err := tx.QueryRowContext(ctx, `SELECT upgrade_id FROM maintenance WHERE singleton=1`).Scan(&v); err != nil {
		return err
	}
	if v != "" {
		return fmt.Errorf("%w: 正在等待系统升级，暂不启动新的运行", model.ErrConflict)
	}
	return nil
}
func (s *Store) RequestUpgradeInstall(ctx context.Context, upgradeID string, version int64, digest string) (model.Upgrade, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Upgrade{}, err
	}
	defer tx.Rollback()
	u, err := getUpgradeTx(ctx, tx, upgradeID)
	if err != nil {
		return u, err
	}
	if digest == u.CandidateSHA256 && digest != "" && (u.State == "WAITING_IDLE" || u.State == "INSTALLING" || u.State == "SUCCEEDED") {
		return u, nil
	}
	if u.State != "READY" || u.Version != version || digest == "" || digest != u.CandidateSHA256 {
		return u, fmt.Errorf("%w: 候选版本已变化或尚未通过测试，请重新查看", model.ErrConflict)
	}
	if err = checkMaintenanceTx(ctx, tx); err != nil {
		return u, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE maintenance SET upgrade_id=? WHERE singleton=1`, u.ID); err != nil {
		return u, err
	}
	u.State = "WAITING_IDLE"
	if err = saveUpgradeTx(ctx, tx, &u, "UpgradeInstallApproved"); err != nil {
		return u, err
	}
	return u, tx.Commit()
}
func (s *Store) BeginUpgradeInstall(ctx context.Context, upgradeID string) (model.Upgrade, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Upgrade{}, err
	}
	defer tx.Rollback()
	u, err := getUpgradeTx(ctx, tx, upgradeID)
	if err != nil {
		return u, err
	}
	if u.State != "WAITING_IDLE" {
		return u, fmt.Errorf("%w: upgrade is not draining", model.ErrConflict)
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE state IN ('QUEUED','RUNNING')`).Scan(&active); err != nil {
		return u, err
	}
	if active > 0 {
		return u, fmt.Errorf("%w: 等待 %d 个运行结束", model.ErrConflict, active)
	}
	u.State = "INSTALLING"
	if err = saveUpgradeTx(ctx, tx, &u, "UpgradeInstalling"); err != nil {
		return u, err
	}
	return u, tx.Commit()
}
func (s *Store) FinishUpgrade(ctx context.Context, upgradeID, state, message, backup string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u, err := getUpgradeTx(ctx, tx, upgradeID)
	if err != nil {
		return err
	}
	if u.State == state {
		return nil
	}
	if u.State != "INSTALLING" && u.State != "WAITING_IDLE" {
		return fmt.Errorf("%w: upgrade not installing", model.ErrConflict)
	}
	if state != "SUCCEEDED" && state != "ROLLED_BACK" && state != "FAILED" {
		return fmt.Errorf("%w: invalid upgrade outcome", model.ErrValidation)
	}
	u.State = state
	u.BackupDir = backup
	u.Error = message
	if _, err = tx.ExecContext(ctx, `UPDATE maintenance SET upgrade_id='' WHERE singleton=1 AND upgrade_id=?`, u.ID); err != nil {
		return err
	}
	if err = saveUpgradeTx(ctx, tx, &u, "Upgrade"+state); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CancelUpgrade(ctx context.Context, upgradeID string, version int64) (model.Upgrade, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Upgrade{}, err
	}
	defer tx.Rollback()
	u, err := getUpgradeTx(ctx, tx, upgradeID)
	if err != nil {
		return u, err
	}
	if u.State == "CANCELLED" {
		return u, nil
	}
	if u.Version != version || !(u.State == "QUEUED" || u.State == "BUILDING" || u.State == "READY" || u.State == "WAITING_IDLE") {
		return u, fmt.Errorf("%w: 当前阶段不能取消，或记录已更新", model.ErrConflict)
	}
	u.State = "CANCELLED"
	if _, err = tx.ExecContext(ctx, `UPDATE maintenance SET upgrade_id='' WHERE singleton=1 AND upgrade_id=?`, u.ID); err != nil {
		return u, err
	}
	if err = saveUpgradeTx(ctx, tx, &u, "UpgradeCancelled"); err != nil {
		return u, err
	}
	return u, tx.Commit()
}
