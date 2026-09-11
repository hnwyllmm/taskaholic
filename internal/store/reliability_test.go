package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"work-assistant/internal/backup"
	"work-assistant/internal/model"
)

func downgradeFixtureToV21(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"improvement_job", "task_evaluation", "session_experience", "experiment_assignment", "improvement_experiment", "improvement_candidate", "experience_revision", "experience", "optimization_policy", "task_profile", "improvement_meta"} {
		if _, err = db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`DELETE FROM schema_version WHERE version=22`); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxFromPreviousEpochCanReplayOnlyOverCurrentConnection(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task, _, err := s.CreateTask(ctx, "key", "Offline task", "retain output across runtime restart")
	if err != nil {
		t.Fatal(err)
	}
	hello := model.RuntimeHello{RuntimeID: "runtime-1", Epoch: "epoch-old", Capabilities: map[string]any{"adapters": map[string]any{"exec-agent": map[string]any{}}}}
	if err := s.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: hello.RuntimeID, AdapterID: "exec-agent", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	event := model.RuntimeEvent{RuntimeID: hello.RuntimeID, Epoch: hello.Epoch, RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.completed", Output: "离线时已保存的结果"}
	hello.Epoch = "epoch-new"
	if err := s.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRuntimeEventFrom(ctx, "epoch-old", event); err == nil {
		t.Fatal("stale connection accepted")
	}
	if _, err := s.ApplyRuntimeEventFrom(ctx, "epoch-new", event); err != nil {
		t.Fatal("durable old outbox cannot replay", err)
	}
	if duplicate, err := s.ApplyRuntimeEventFrom(ctx, "epoch-new", event); err != nil || !duplicate {
		t.Fatal("replay not idempotent", err)
	}
	saved, err := s.GetRun(ctx, run.ID)
	if err != nil || saved.Output != event.Output || saved.State != model.RunStateCompleted {
		t.Fatalf("lost output: %+v %v", saved, err)
	}
}

func TestProductionOpenRefusesMissingData(t *testing.T) {
	t.Setenv("ASSISTANT_BACKUP_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := OpenProtected(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenProtected(path); err == nil {
		reopened.Close()
		t.Fatal("production open created an empty replacement")
	}
}

func TestProtectedMigrationRecordsVerifiedRecoveryPoint(t *testing.T) {
	t.Setenv("ASSISTANT_BACKUP_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeFixtureToV21(t, path)
	s, err = OpenProtected(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var raw []byte
	if err = s.db.QueryRow(`SELECT value FROM improvement_meta WHERE key='schema-migration:22'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var audit struct {
		RecoveryPointID string `json:"recovery_point_id"`
		Before          int    `json:"schema_before"`
		After           int    `json:"schema_after"`
		Integrity       string `json:"integrity_check"`
		ForeignKeys     string `json:"foreign_key_check"`
	}
	if err = json.Unmarshal(raw, &audit); err != nil || audit.RecoveryPointID == "" || audit.Before != 21 || audit.After != 22 || audit.Integrity != "ok" || audit.ForeignKeys != "ok" {
		t.Fatalf("migration audit: %#v, %v", audit, err)
	}
	var events int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE event_type='SchemaMigrated' AND causation_id=?`, audit.RecoveryPointID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("migration event missing: %d, %v", events, err)
	}
}

func versionlessRuntimeFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE inbound_message(message_id TEXT PRIMARY KEY, params_json TEXT NOT NULL)",
		"CREATE TABLE outbound_message(runtime_seq INTEGER PRIMARY KEY, params_json TEXT)",
		"CREATE TABLE local_run(run_id TEXT PRIMARY KEY, spec_json TEXT NOT NULL)",
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCombinedMigrationUsesExactVerifiedRecoveryPoint(t *testing.T) {
	t.Setenv("ASSISTANT_BACKUP_DIR", t.TempDir())
	dataDirectory := t.TempDir()
	controlPath := filepath.Join(dataDirectory, "control.sqlite")
	runtimePath := filepath.Join(dataDirectory, "runtime.sqlite")
	s, err := Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeFixtureToV21(t, controlPath)
	versionlessRuntimeFixture(t, runtimePath)
	backupRoot, err := backup.Directory(dataDirectory)
	if err != nil {
		t.Fatal(err)
	}
	point, created, err := backup.CapturePreMigrationSet(context.Background(), controlPath, backupRoot, SchemaVersion, map[string]backup.Source{
		"control.sqlite": backup.DatabaseSource{Path: controlPath},
		"runtime.sqlite": backup.DatabaseSource{Path: runtimePath},
	})
	if err != nil || !created {
		t.Fatalf("combined migration point: %#v, created=%v, err=%v", point, created, err)
	}
	s, err = OpenProtectedWithRecoveryPoint(controlPath, point)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var raw []byte
	if err = s.db.QueryRow(`SELECT value FROM improvement_meta WHERE key='schema-migration:22'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var audit struct {
		RecoveryPointID string `json:"recovery_point_id"`
	}
	if err = json.Unmarshal(raw, &audit); err != nil || audit.RecoveryPointID != point.ID {
		t.Fatalf("migration did not audit the combined point: %#v, %v", audit, err)
	}
	entries, err := os.ReadDir(filepath.Join(backupRoot, "pre-migration"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("combined gate created a redundant recovery point: %v, %v", entries, err)
	}
}

func TestCorruptCombinedRecoveryPointRefusesMigrationAndLeavesV21(t *testing.T) {
	t.Setenv("ASSISTANT_BACKUP_DIR", t.TempDir())
	dataDirectory := t.TempDir()
	controlPath := filepath.Join(dataDirectory, "control.sqlite")
	runtimePath := filepath.Join(dataDirectory, "runtime.sqlite")
	s, err := Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	downgradeFixtureToV21(t, controlPath)
	versionlessRuntimeFixture(t, runtimePath)
	backupRoot, _ := backup.Directory(dataDirectory)
	point, created, err := backup.CapturePreMigrationSet(context.Background(), controlPath, backupRoot, SchemaVersion, map[string]backup.Source{
		"control.sqlite": backup.DatabaseSource{Path: controlPath},
		"runtime.sqlite": backup.DatabaseSource{Path: runtimePath},
	})
	if err != nil || !created {
		t.Fatal(point, created, err)
	}
	if err = os.WriteFile(filepath.Join(backupRoot, "pre-migration", point.ID, "control.sqlite"), []byte("damaged recovery point"), 0o600); err != nil {
		t.Fatal(err)
	}
	if migrated, openErr := OpenProtectedWithRecoveryPoint(controlPath, point); openErr == nil {
		migrated.Close()
		t.Fatal("migration started with a corrupted combined recovery point")
	}
	db, err := sql.Open("sqlite", controlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, improvementTables int
	if err = db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='improvement_candidate'`).Scan(&improvementTables); err != nil {
		t.Fatal(err)
	}
	if version != 21 || improvementTables != 0 {
		t.Fatalf("corrupt backup allowed schema change: version=%d tables=%d", version, improvementTables)
	}
}

func TestV22MigrationDoesNotRewriteHistoricalTaskOrSummary(t *testing.T) {
	t.Setenv("ASSISTANT_BACKUP_DIR", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterRuntime(ctx, model.RuntimeHello{RuntimeID: "migration-runtime", Epoch: "migration-epoch", Capabilities: map[string]any{"adapters": map[string]any{"exec-agent": map[string]any{}}}}); err != nil {
		t.Fatal(err)
	}
	task, _, err := s.CreateTask(ctx, "migration-history", "Historical task", "keep immutable history")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: "migration-runtime", AdapterID: "exec-agent", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "migration-epoch", RuntimeSeq: 1, RunID: run.ID, TaskID: task.ID, Type: "run.completed", Output: "historical result"}); err != nil {
		t.Fatal(err)
	}
	var beforeSummary, beforeTask []byte
	if err = s.db.QueryRow(`SELECT data_json FROM task_summary WHERE task_id=?`, task.ID).Scan(&beforeSummary); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT json_object('title',title,'goal',goal,'state',state,'version',version,'revision',current_revision_id,'created',created_at_ms,'updated',updated_at_ms,'requirements',json(requirements_json)) FROM task WHERE task_id=?`, task.ID).Scan(&beforeTask); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeFixtureToV21(t, path)
	s, err = OpenProtected(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var afterSummary, afterTask []byte
	if err = s.db.QueryRow(`SELECT data_json FROM task_summary WHERE task_id=?`, task.ID).Scan(&afterSummary); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT json_object('title',title,'goal',goal,'state',state,'version',version,'revision',current_revision_id,'created',created_at_ms,'updated',updated_at_ms,'requirements',json(requirements_json)) FROM task WHERE task_id=?`, task.ID).Scan(&afterTask); err != nil {
		t.Fatal(err)
	}
	if string(beforeSummary) != string(afterSummary) || string(beforeTask) != string(afterTask) {
		t.Fatalf("v22 migration rewrote historical data\nsummary before=%s\nsummary after=%s\ntask before=%s\ntask after=%s", beforeSummary, afterSummary, beforeTask, afterTask)
	}
}

func TestMigrationFailureRollsBackAndKeepsRecoveryPoint(t *testing.T) {
	t.Setenv("ASSISTANT_BACKUP_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	downgradeFixtureToV21(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE optimization_policy(broken TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if migrated, openErr := OpenProtected(path); openErr == nil {
		migrated.Close()
		t.Fatal("injected migration failure was ignored")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, newTables int
	if err = db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='task_profile'`).Scan(&newTables); err != nil {
		t.Fatal(err)
	}
	if version != 21 || newTables != 0 {
		t.Fatalf("failed migration was not rolled back: schema=%d task_profile=%d", version, newTables)
	}
	backupRoot, _ := backup.Directory(filepath.Dir(path))
	entries, err := os.ReadDir(filepath.Join(backupRoot, "pre-migration"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("verified recovery point was lost: %v, %v", entries, err)
	}
	pointDirectory := filepath.Join(backupRoot, "pre-migration", entries[0].Name())
	if _, err = backup.Verify(context.Background(), pointDirectory); err != nil {
		t.Fatalf("recovery point no longer verifies after migration failure: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "restored-copy")
	if _, err = backup.Restore(context.Background(), pointDirectory, restored); err != nil {
		t.Fatalf("recovery point is not restorable into a new directory: %v", err)
	}
}

func TestPostMigrationValidationFailureFencesDatabase(t *testing.T) {
	t.Setenv("ASSISTANT_BACKUP_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	downgradeFixtureToV21(t, path)
	previous := validateMigratedDatabase
	validateMigratedDatabase = func(*sql.DB) error { return errors.New("injected integrity failure") }
	_, err = OpenProtected(path)
	validateMigratedDatabase = previous
	if !errors.Is(err, ErrPostMigrationValidation) {
		t.Fatalf("post-migration failure not reported: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(path), "RECOVERY_REQUIRED")); statErr != nil {
		t.Fatalf("database was not fenced: %v", statErr)
	}
	if reopened, openErr := OpenProtected(path); openErr == nil {
		reopened.Close()
		t.Fatal("fenced database restarted")
	}
}
