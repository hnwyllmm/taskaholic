// Package backup owns verified, local recovery points. It does not depend on
// the scheduler, store schema, or an Agent's native session implementation.
package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	return sql.Open("sqlite", u.String()+"?mode=ro")
}

// VerifySQLite must not migrate a backup or silently create a missing database.
// Do not use immutable=1: a live source may have committed data in its WAL.
func VerifySQLite(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("not a non-empty regular database: %s", path)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	ok := false
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			rows.Close()
			return err
		}
		if result != "ok" {
			rows.Close()
			return fmt.Errorf("SQLite integrity check: %s", result)
		}
		ok = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("SQLite integrity check returned no result")
	}
	rows, err = db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("SQLite foreign key check failed")
	}
	return rows.Err()
}

// SQLiteSnapshot includes committed WAL contents, validates and fsyncs the
// result, then publishes without ever replacing an existing destination.
func SQLiteSnapshot(ctx context.Context, db *sql.DB, destination string) error {
	if destination == "" {
		return errors.New("backup destination is required")
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".sqlite-partial-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	path := filepath.Join(stage, "snapshot.sqlite")
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("snapshot SQLite: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if err := VerifySQLite(ctx, path); err != nil {
		return err
	}
	if err := syncFile(path); err != nil {
		return err
	}
	if err := os.Link(path, destination); err != nil {
		return err
	}
	return syncDir(parent)
}

type DatabaseSource struct{ Path string }

func (s DatabaseSource) Backup(ctx context.Context, destination string) error {
	if err := VerifySQLite(ctx, s.Path); err != nil {
		return err
	}
	db, err := openReadOnly(s.Path)
	if err != nil {
		return err
	}
	defer db.Close()
	return SQLiteSnapshot(ctx, db, destination)
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDir(path string) error { return syncFile(path) }

func SecureSQLiteFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			return err
		}
	}
	return nil
}

func digestFile(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func copyExclusive(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}
