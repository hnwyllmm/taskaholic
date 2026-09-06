package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source is the extension point for future storage / Agent session providers.
// This release only accepts consistent SQLite sources, not live directory copies.
type Source interface {
	Backup(context.Context, string) error
}

type Config struct {
	Directory  string
	Interval   time.Duration
	Sources    map[string]Source
	KeepRecent int
	KeepDays   int
}

type File struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Snapshot struct {
	Format     int       `json:"format"`
	ID         string    `json:"id"`
	Reason     string    `json:"reason"`
	CreatedAt  time.Time `json:"created_at"`
	VerifiedAt time.Time `json:"verified_at"`
	Files      []File    `json:"files"`
}

type Status struct {
	Enabled         bool       `json:"enabled"`
	Directory       string     `json:"directory"`
	IntervalSeconds int64      `json:"interval_seconds"`
	Running         bool       `json:"running"`
	LastAttempt     time.Time  `json:"last_attempt"`
	LastSuccess     time.Time  `json:"last_success"`
	LastError       string     `json:"last_error"`
	Snapshots       []Snapshot `json:"snapshots"`
	OffDevice       bool       `json:"off_device"` // Never assume a path means a different physical disk.
}

type Manager struct {
	config    Config
	captureMu sync.Mutex
	mu        sync.Mutex
	status    Status
}

func New(config Config) (*Manager, error) {
	if config.Directory == "" || len(config.Sources) == 0 {
		return nil, errors.New("backup directory and sources are required")
	}
	var err error
	config.Directory, err = filepath.Abs(config.Directory)
	if err != nil {
		return nil, err
	}
	if config.Interval == 0 {
		config.Interval = 5 * time.Minute
	}
	if config.Interval < time.Second {
		return nil, errors.New("backup interval must be at least one second")
	}
	if config.KeepRecent == 0 {
		config.KeepRecent = 48
	}
	if config.KeepDays == 0 {
		config.KeepDays = 30
	}
	if config.KeepRecent < 1 || config.KeepDays < 1 {
		return nil, errors.New("backup retention must be positive")
	}
	sources := make(map[string]Source, len(config.Sources))
	for name, source := range config.Sources {
		if !safeDBName(name) || source == nil {
			return nil, fmt.Errorf("invalid backup source: %s", name)
		}
		sources[name] = source
	}
	config.Sources = sources
	if err := os.MkdirAll(config.Directory, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{config: config, status: Status{Enabled: true, Directory: config.Directory, IntervalSeconds: int64(config.Interval.Seconds()), Snapshots: []Snapshot{}}}
	// A remembered successful timestamp is not evidence after restart: verify
	// the newest matching recovery point again before reporting it as healthy.
	items, err := m.list()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, item := range items {
		if _, err := Verify(ctx, filepath.Join(config.Directory, item.ID)); err != nil {
			m.status.LastError = "existing recovery point failed verification: " + err.Error()
			continue
		}
		m.status.LastSuccess = item.CreatedAt
		break
	}
	m.status.Snapshots = recent(items)
	return m, nil
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := m.status
	result.Snapshots = append([]Snapshot{}, result.Snapshots...)
	return result
}

// Run is owned by the application's lifecycle, not an external reminder.
// Call Capture once before starting dispatch; a first backup failure is fatal.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.Capture(ctx, "scheduled"); err != nil && ctx.Err() == nil {
				slog.Error("automatic backup failed", "error", err)
			}
		}
	}
}

func (m *Manager) Capture(ctx context.Context, reason string) (snapshot Snapshot, err error) {
	if !m.captureMu.TryLock() {
		return snapshot, errors.New("a backup is already running")
	}
	defer m.captureMu.Unlock()
	m.mu.Lock()
	m.status.Running, m.status.LastAttempt = true, time.Now().UTC()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.status.Running = false
		if err != nil {
			m.status.LastError = err.Error()
		} else {
			m.status.LastError = ""
		}
		m.mu.Unlock()
	}()
	stage, err := os.MkdirTemp(m.config.Directory, ".partial-")
	if err != nil {
		return snapshot, err
	}
	defer os.RemoveAll(stage)
	now := time.Now().UTC()
	snapshot = Snapshot{Format: 1, ID: "snapshot-" + now.Format("20060102T150405.000000000Z") + "-" + strings.TrimPrefix(filepath.Base(stage), ".partial-"), Reason: reason, CreatedAt: now}
	var names []string
	for name := range m.config.Sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return snapshot, err
		}
		path := filepath.Join(stage, name)
		if err := m.config.Sources[name].Backup(ctx, path); err != nil {
			return snapshot, fmt.Errorf("backup %s: %w", name, err)
		}
		if err := VerifySQLite(ctx, path); err != nil {
			return snapshot, fmt.Errorf("verify %s: %w", name, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return snapshot, err
		}
		if err := syncFile(path); err != nil {
			return snapshot, err
		}
		digest, size, err := digestFile(path)
		if err != nil {
			return snapshot, err
		}
		snapshot.Files = append(snapshot.Files, File{Name: name, SHA256: digest, Size: size})
	}
	snapshot.VerifiedAt = time.Now().UTC()
	if err := writeJSONExclusive(filepath.Join(stage, "manifest.json"), snapshot); err != nil {
		return snapshot, err
	}
	if _, err := Verify(ctx, stage); err != nil {
		return snapshot, err
	}
	if err := syncDir(stage); err != nil {
		return snapshot, err
	}
	if err := os.Rename(stage, filepath.Join(m.config.Directory, snapshot.ID)); err != nil {
		return snapshot, err
	}
	if err := syncDir(m.config.Directory); err != nil {
		return snapshot, err
	}
	m.mu.Lock()
	m.status.LastSuccess = snapshot.CreatedAt
	m.mu.Unlock()
	// Pruning is allowed only after publishing a new, fully verified snapshot.
	items, err := m.list()
	if err != nil {
		return snapshot, err
	}
	if err := m.prune(ctx, items, now); err != nil {
		return snapshot, fmt.Errorf("snapshot saved; retention failed: %w", err)
	}
	items, err = m.list()
	if err != nil {
		return snapshot, err
	}
	m.mu.Lock()
	m.status.Snapshots = recent(items)
	m.mu.Unlock()
	return snapshot, nil
}

func safeDBName(name string) bool {
	return name != "" && filepath.Base(name) == name && !strings.ContainsAny(name, "\\/\x00") && strings.HasSuffix(name, ".sqlite") && !strings.HasPrefix(name, ".")
}

func readManifest(directory string) (Snapshot, error) {
	var snapshot Snapshot
	info, err := os.Lstat(directory)
	if err != nil {
		return snapshot, err
	}
	if !info.IsDir() {
		return snapshot, errors.New("snapshot must be a real directory, not a symlink")
	}
	path := filepath.Join(directory, "manifest.json")
	info, err = os.Lstat(path)
	if err != nil {
		return snapshot, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return snapshot, errors.New("invalid backup manifest file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return snapshot, err
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return snapshot, err
	}
	if snapshot.Format != 1 || !strings.HasPrefix(snapshot.ID, "snapshot-") || filepath.Base(snapshot.ID) != snapshot.ID || strings.ContainsAny(snapshot.ID, "\\/\x00") || len(snapshot.Files) == 0 || snapshot.CreatedAt.IsZero() || snapshot.VerifiedAt.IsZero() {
		return snapshot, errors.New("invalid or unsupported backup manifest")
	}
	seen := map[string]bool{}
	for _, f := range snapshot.Files {
		if !safeDBName(f.Name) || seen[f.Name] || f.Size <= 0 || len(f.SHA256) != 64 {
			return snapshot, errors.New("invalid backup file entry")
		}
		seen[f.Name] = true
	}
	return snapshot, nil
}

func Verify(ctx context.Context, directory string) (Snapshot, error) {
	snapshot, err := readManifest(directory)
	if err != nil {
		return snapshot, err
	}
	for _, f := range snapshot.Files {
		if err := ctx.Err(); err != nil {
			return snapshot, err
		}
		path := filepath.Join(directory, f.Name)
		digest, size, err := digestFile(path)
		if err != nil {
			return snapshot, err
		}
		if digest != f.SHA256 || size != f.Size {
			return snapshot, fmt.Errorf("backup checksum mismatch: %s", f.Name)
		}
		if err := VerifySQLite(ctx, path); err != nil {
			return snapshot, fmt.Errorf("backup integrity: %s: %w", f.Name, err)
		}
	}
	return snapshot, nil
}

// Restore never overwrites live data. The new directory is deliberately fenced:
// restoring two independent DB snapshots does not create a distributed atomic
// checkpoint, and pending work must be reconciled before anything can run.
func Restore(ctx context.Context, directory, destination string) (Snapshot, error) {
	snapshot, err := Verify(ctx, directory)
	if err != nil {
		return snapshot, err
	}
	if destination == "" {
		return snapshot, errors.New("restore destination is required")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return snapshot, fmt.Errorf("restore requires a new directory: %w", err)
	}
	if err := writeJSONExclusive(filepath.Join(destination, "RECOVERY_REQUIRED"), map[string]any{"snapshot": snapshot.ID, "warning": "Offline recovery only. Reconcile active runs, pending commands/events, workspaces and native Agent sessions before removing this fence."}); err != nil {
		return snapshot, err
	}
	if err := syncDir(destination); err != nil {
		return snapshot, err
	}
	for _, f := range snapshot.Files {
		if err := ctx.Err(); err != nil {
			return snapshot, err
		}
		if err := copyExclusive(filepath.Join(directory, f.Name), filepath.Join(destination, f.Name)); err != nil {
			return snapshot, err
		}
	}
	if err := writeJSONExclusive(filepath.Join(destination, "manifest.json"), snapshot); err != nil {
		return snapshot, err
	}
	if _, err := Verify(ctx, destination); err != nil {
		return snapshot, err
	}
	return snapshot, errors.Join(syncDir(destination), syncDir(filepath.Dir(destination)))
}

func writeJSONExclusive(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(raw, '\n'))
	syncErr := f.Sync()
	return errors.Join(writeErr, syncErr, f.Close())
}

func (m *Manager) list() ([]Snapshot, error) {
	entries, err := os.ReadDir(m.config.Directory)
	if err != nil {
		return nil, err
	}
	items := []Snapshot{}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "snapshot-") {
			continue
		}
		item, err := readManifest(filepath.Join(m.config.Directory, entry.Name()))
		if err != nil || item.ID != entry.Name() || len(item.Files) != len(m.config.Sources) {
			continue
		}
		matches := true
		for _, f := range item.Files {
			if m.config.Sources[f.Name] == nil {
				matches = false
			}
		}
		if matches {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items, nil
}

func recent(items []Snapshot) []Snapshot {
	if len(items) > 10 {
		items = items[:10]
	}
	return append([]Snapshot{}, items...)
}

func (m *Manager) prune(ctx context.Context, items []Snapshot, now time.Time) error {
	days := map[string]bool{}
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		day := item.CreatedAt.UTC().Format("2006-01-02")
		keepDaily := !days[day] && item.CreatedAt.After(now.AddDate(0, 0, -m.config.KeepDays))
		days[day] = true
		if i < m.config.KeepRecent || keepDaily || item.Reason == "manual" {
			continue
		}
		path := filepath.Join(m.config.Directory, item.ID)
		// Only remove exactly the files in our own manifest. Unknown files,
		// symlinks, manual exports and damaged snapshots are left for inspection.
		if _, err := Verify(ctx, path); err != nil {
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) != len(item.Files)+1 {
			continue
		}
		allowed := map[string]bool{"manifest.json": true}
		for _, f := range item.Files {
			allowed[f.Name] = true
		}
		valid := true
		for _, e := range entries {
			if !allowed[e.Name()] || !e.Type().IsRegular() {
				valid = false
			}
		}
		if !valid {
			continue
		}
		for _, e := range entries {
			if err := os.Remove(filepath.Join(path, e.Name())); err != nil {
				return err
			}
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return syncDir(m.config.Directory)
}
