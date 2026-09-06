package backup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Directory is outside the application / workspace by default, so removing or
// replacing a release cannot also remove its recovery points. A user-supplied
// directory can live on a separately managed disk; no data is uploaded.
func Directory(dataDir string) (string, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	base := os.Getenv("ASSISTANT_BACKUP_DIR")
	if base == "" {
		base, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(base, "WorkAssistant", "backups")
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(abs))
	// Retention for one installation must never prune another installation's
	// recovery points, including when they share a configured external root.
	return filepath.Join(base, fmt.Sprintf("%x", key[:12])), nil
}

func markerPaths(path, directory string) ([]string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(abs))
	return []string{filepath.Join(directory, fmt.Sprintf("source-%x.json", key[:12])), filepath.Join(filepath.Dir(abs), ".initialized-"+filepath.Base(abs)+".json")}, nil
}

// CheckBeforeOpen fails closed on corruption, missing previously registered
// data, unfinished restoration, or a newer schema. Never auto-restore / replay.
func CheckBeforeOpen(ctx context.Context, path, directory string, schema int, requiredTables ...string) error {
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), "RECOVERY_REQUIRED")); err == nil {
		return errors.New("restored data is fenced by RECOVERY_REQUIRED; reconcile pending work before starting")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	markers, err := markerPaths(path, directory)
	if err != nil {
		return err
	}
	registered := false
	for _, marker := range markers {
		if _, err := os.Lstat(marker); err == nil {
			registered = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if registered {
			return fmt.Errorf("registered database is missing: %s; refusing to create an empty replacement; backups: %s", path, directory)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Lstat(path + suffix); err == nil {
				return fmt.Errorf("database is missing but %s exists; preserve it for recovery", path+suffix)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("database is empty or not a regular file: %s; refusing initialization", path)
	}
	if err := VerifySQLite(ctx, path); err != nil {
		return fmt.Errorf("database validation failed, original data preserved: %w", err)
	}
	if len(requiredTables) > 0 {
		db, err := openReadOnly(path)
		if err != nil {
			return err
		}
		defer db.Close()
		for _, name := range requiredTables {
			var count int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				return fmt.Errorf("required table %s is missing; refusing to initialize over existing data", name)
			}
		}
	}
	if schema > 0 {
		db, err := openReadOnly(path)
		if err != nil {
			return err
		}
		var version int
		err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version)
		db.Close()
		if err != nil {
			return fmt.Errorf("unrecognized database schema: %w", err)
		}
		if version > schema {
			return fmt.Errorf("database schema %d is newer than supported schema %d; downgrade refused", version, schema)
		}
		if version < schema {
			m, err := New(Config{Directory: filepath.Join(directory, "pre-migration"), Sources: map[string]Source{filepath.Base(path): DatabaseSource{Path: path}}})
			if err != nil {
				return err
			}
			if _, err := m.Capture(ctx, fmt.Sprintf("before-schema-%d-to-%d", version, schema)); err != nil {
				return fmt.Errorf("required pre-migration backup failed: %w", err)
			}
		}
	}
	return nil
}

func Register(path, directory string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	markers, err := markerPaths(path, directory)
	if err != nil {
		return err
	}
	for _, marker := range markers {
		raw, err := os.ReadFile(marker)
		if err == nil {
			var saved struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(raw, &saved) != nil || saved.Path != abs {
				return fmt.Errorf("source identity mismatch: %s", marker)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := writeJSONExclusive(marker, map[string]any{"path": abs, "format": 1}); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(marker)); err != nil {
			return err
		}
	}
	return nil
}
