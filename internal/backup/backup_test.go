package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT)", "INSERT INTO notes VALUES(1, '必须保存的任务总结')", "CREATE TABLE schema_version(version INTEGER)", "INSERT INTO schema_version VALUES(1)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func TestSnapshotIncludesCommittedWALAndExcludesUncommittedChanges(t *testing.T) {
	db, path := fixture(t)
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture must have WAL: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO notes VALUES(2, 'not committed')"); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "copy.sqlite")
	if err := (DatabaseSource{Path: path}).Backup(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	copyDB, err := openReadOnly(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var count int
	var body string
	if err := copyDB.QueryRow("SELECT COUNT(*), MAX(body) FROM notes").Scan(&count, &body); err != nil {
		t.Fatal(err)
	}
	if count != 1 || body != "必须保存的任务总结" {
		t.Fatalf("lost WAL or included uncommitted data: %d %q", count, body)
	}
	before, _, _ := digestFile(destination)
	if err := (DatabaseSource{Path: path}).Backup(context.Background(), destination); err == nil {
		t.Fatal("overwrote existing snapshot")
	}
	after, _, _ := digestFile(destination)
	if before != after {
		t.Fatal("existing destination changed")
	}
	info, _ := os.Stat(destination)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("backup not private", info.Mode())
	}
}

type injectedSource struct {
	path string
	fail bool
}

func (s *injectedSource) Backup(ctx context.Context, destination string) error {
	if s.fail {
		if err := os.WriteFile(destination, []byte("partial disk write"), 0o600); err != nil {
			return err
		}
		return errors.New("injected disk failure")
	}
	return (DatabaseSource{Path: s.path}).Backup(ctx, destination)
}

func TestRecoveryPointFailureDoesNotPublishOrReplaceLastGood(t *testing.T) {
	_, path := fixture(t)
	source := &injectedSource{path: path}
	m, err := New(Config{Directory: t.TempDir(), Sources: map[string]Source{"control.sqlite": source}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := m.Capture(ctx, "startup")
	if err != nil {
		t.Fatal(err)
	}
	source.fail = true
	if _, err := m.Capture(ctx, "scheduled"); err == nil {
		t.Fatal("expected disk failure")
	}
	status := m.Status()
	if status.Running || status.LastError == "" || !status.LastSuccess.Equal(first.CreatedAt) || len(status.Snapshots) != 1 {
		t.Fatalf("bad failure status: %+v", status)
	}
	entries, err := os.ReadDir(m.config.Directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("partial snapshot published or not cleaned: %v %v", entries, err)
	}
	if _, err := Verify(ctx, filepath.Join(m.config.Directory, first.ID)); err != nil {
		t.Fatal(err)
	}
	// Reopening validates, instead of trusting an in-memory / cached timestamp.
	reopened, err := New(m.config)
	if err != nil || reopened.Status().LastSuccess.IsZero() {
		t.Fatalf("reopen: %v", err)
	}
}

func TestRestoreVerifiesChecksumsNeverOverwritesAndFencesExecution(t *testing.T) {
	_, path := fixture(t)
	m, err := New(Config{Directory: t.TempDir(), Sources: map[string]Source{"control.sqlite": DatabaseSource{Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := m.Capture(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(m.config.Directory, snapshot.ID)
	to := filepath.Join(t.TempDir(), "restore")
	if _, err := Restore(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, to); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, from, to); err == nil {
		t.Fatal("restore overwrote an existing directory")
	}
	if err := CheckBeforeOpen(ctx, filepath.Join(to, "control.sqlite"), m.config.Directory, 1); err == nil || !strings.Contains(err.Error(), "RECOVERY_REQUIRED") {
		t.Fatalf("restore started work: %v", err)
	}
	if err := os.WriteFile(filepath.Join(from, "control.sqlite"), []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	badTarget := filepath.Join(t.TempDir(), "invalid")
	if _, err := Restore(ctx, from, badTarget); err == nil {
		t.Fatal("restored corrupted snapshot")
	}
	if _, err := os.Stat(badTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid restore created destination")
	}
}

func TestManifestRejectsTraversalSymlinksAndForeignKeyDamage(t *testing.T) {
	db, path := fixture(t)
	if _, err := db.Exec("CREATE TABLE child(note_id INTEGER REFERENCES notes(id)); INSERT INTO child VALUES(100)"); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLite(context.Background(), path); err == nil {
		t.Fatal("missed foreign key corruption")
	}
	dir := t.TempDir()
	manifest := Snapshot{Format: 1, ID: "snapshot-test", CreatedAt: time.Now(), VerifiedAt: time.Now(), Files: []File{{Name: "../outside.sqlite", Size: 10, SHA256: strings.Repeat("a", 64)}}}
	if err := writeJSONExclusive(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), dir); err == nil {
		t.Fatal("accepted traversal")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLite(context.Background(), link); err == nil {
		t.Fatal("accepted database symlink")
	}
}

func TestGuardPreventsSilentEmptyDatabaseAndBacksUpBeforeMigration(t *testing.T) {
	db, path := fixture(t)
	root := t.TempDir()
	ctx := context.Background()
	if err := CheckBeforeOpen(ctx, path, root, 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "pre-migration"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("no pre-migration snapshot: %v %v", entries, err)
	}
	if _, err := Verify(ctx, filepath.Join(root, "pre-migration", entries[0].Name())); err != nil {
		t.Fatal(err)
	}
	if err := Register(path, root); err != nil {
		t.Fatal(err)
	}
	if err := CheckBeforeOpen(ctx, path, root, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO schema_version VALUES(99)"); err != nil {
		t.Fatal(err)
	}
	if err := CheckBeforeOpen(ctx, path, root, 2); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("accepted newer schema: %v", err)
	}
	db.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := CheckBeforeOpen(ctx, path, root, 1); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("silently recreated data: %v", err)
	}
	// The local marker also protects against accidentally changing backup roots.
	if err := CheckBeforeOpen(ctx, path, t.TempDir(), 1); err == nil {
		t.Fatal("changing backup root bypassed missing-data protection")
	}
	// If the data directory itself disappears, the outside marker still works.
	markers, _ := markerPaths(path, root)
	if err := os.Remove(markers[1]); err != nil {
		t.Fatal(err)
	}
	if err := CheckBeforeOpen(ctx, path, root, 1); err == nil {
		t.Fatal("external registration was ignored")
	}
}

func TestGuardRejectsEmptyCorruptAndOrphanWAL(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, contents := range [][]byte{nil, []byte("corrupt data")} {
		path := filepath.Join(t.TempDir(), "control.sqlite")
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CheckBeforeOpen(ctx, path, root, 0); err == nil {
			t.Fatal("accepted empty/corrupt DB")
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != string(contents) {
			t.Fatal("damaged database was modified")
		}
	}
	path := filepath.Join(t.TempDir(), "control.sqlite")
	if err := os.WriteFile(path+"-wal", []byte("committed state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckBeforeOpen(ctx, path, root, 0); err == nil {
		t.Fatal("ignored orphan WAL")
	}
}

func TestRetentionOnlyPrunesManagedVerifiedAutomaticSnapshotsAfterSuccess(t *testing.T) {
	_, path := fixture(t)
	source := &injectedSource{path: path}
	m, err := New(Config{Directory: t.TempDir(), Sources: map[string]Source{"control.sqlite": source}, KeepRecent: 2, KeepDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	automatic, _ := m.Capture(ctx, "startup")
	manual, _ := m.Capture(ctx, "manual")
	m.config.KeepRecent = 1
	for _, s := range []Snapshot{automatic, manual} {
		s.CreatedAt = time.Now().AddDate(0, 0, -40)
		raw, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(m.config.Directory, s.ID, "manifest.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	userFile := filepath.Join(m.config.Directory, "my-own-backup.sqlite")
	if err := os.WriteFile(userFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.fail = true
	if _, err := m.Capture(ctx, "scheduled"); err == nil {
		t.Fatal("expected injected failure")
	}
	if _, err := os.Stat(filepath.Join(m.config.Directory, automatic.ID)); err != nil {
		t.Fatal("failure pruned old snapshot")
	}
	source.fail = false
	if _, err := m.Capture(ctx, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.config.Directory, automatic.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old automatic snapshot was not pruned")
	}
	if _, err := Verify(ctx, filepath.Join(m.config.Directory, manual.ID)); err != nil {
		t.Fatal("manual snapshot was pruned", err)
	}
	if _, err := os.Stat(userFile); err != nil {
		t.Fatal("user file was removed", err)
	}
}

func TestScheduledCaptureAndCancellation(t *testing.T) {
	_, path := fixture(t)
	m, err := New(Config{Directory: t.TempDir(), Interval: time.Second, Sources: map[string]Source{"control.sqlite": DatabaseSource{Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(4 * time.Second)
	for m.Status().LastSuccess.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("automatic backup did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	before := m.Status().LastSuccess
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := m.Capture(cancelled, "scheduled"); err == nil {
		t.Fatal("ignored cancellation")
	}
	if !m.Status().LastSuccess.Equal(before) {
		t.Fatal("cancelled capture changed last good checkpoint")
	}
}

func TestConfiguredBackupRootIsNamespacedPerInstallation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ASSISTANT_BACKUP_DIR", root)
	first, err := Directory(filepath.Join(t.TempDir(), "first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Directory(filepath.Join(t.TempDir(), "second"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || filepath.Dir(first) != root || filepath.Dir(second) != root {
		t.Fatal("shared backup root mixed installations", first, second)
	}
}
