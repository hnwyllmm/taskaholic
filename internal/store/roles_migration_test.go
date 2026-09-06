package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestV3MigrationPreservesLegacyTasksAndSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	task, _, err := s.CreateTask(ctx, "legacy", "legacy task", "existing work")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: "legacy-runtime", AdapterID: "exec-agent", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	// Construct a v3 fixture with real records, removing only v4 additions.
	for _, statement := range []string{
		`DROP TABLE agent_profile`, `DROP TABLE role_draft`, `DROP TABLE role`,
		`DROP INDEX run_agent_state_idx`,
		`ALTER TABLE task DROP COLUMN requirements_json`,
		`ALTER TABLE run DROP COLUMN role_snapshot_json`,
		`ALTER TABLE run DROP COLUMN output`,
		`DELETE FROM schema_version WHERE version = 4`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil || got.Title != task.Title || !got.Requirements.IsEmpty() {
		t.Fatalf("migrated task: %+v %v", got, err)
	}
	gotRun, err := s.GetRun(ctx, run.ID)
	if err != nil || gotRun.SessionID != run.SessionID || gotRun.AgentID != run.AgentID || gotRun.Role != nil || gotRun.Output != "" {
		t.Fatalf("migrated run: %+v %v", gotRun, err)
	}
	if _, err := s.GetSessionDetail(ctx, run.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("migration is not repeatable: %v", err)
	}
	if _, err := s.CreateRoleDraft(ctx, "new capability", "", ""); err != nil {
		t.Fatal(err)
	}
}
