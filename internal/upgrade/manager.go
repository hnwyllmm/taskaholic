// Package upgrade builds application changes in an isolated copy. Installation
// is deliberately separate so a human can inspect and approve an immutable
// candidate before the supervisor changes the live release.
package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"work-assistant/internal/agent"
	protection "work-assistant/internal/backup"
	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

const maxSourceBytes = 32 << 20

var sourceTargets = []string{"cmd", "internal", "go.mod", "go.sum", "README.md", "HOME-PAGES-VERIFICATION.md", "WORKBENCH-VERIFICATION.md"}

type Config struct {
	RuntimeID    string
	Root         string
	DataDir      string
	GoBinary     string
	NodeBinary   string
	ModelID      string
	CodexBinary  string
	CursorBinary string
	AdapterID    string
	// ValidationSandbox can be replaced by another OS/container executor. When
	// nil, select macOS Seatbelt or Linux bubblewrap; never run unconfined.
	ValidationSandbox ValidationSandbox
}

type Manager struct {
	config      Config
	adapter     agent.Adapter
	sandbox     ValidationSandbox
	moduleCache string
}

func New(config Config, adapter agent.Adapter) (*Manager, error) {
	var err error
	config.Root, err = filepath.Abs(config.Root)
	if err != nil {
		return nil, err
	}
	config.DataDir, err = filepath.Abs(config.DataDir)
	if err != nil {
		return nil, err
	}
	if config.AdapterID == "" {
		config.AdapterID = "codex-agent"
	}
	if adapter == nil {
		adapter, err = newBuilderAdapter(config, config.AdapterID)
		if err != nil {
			return nil, err
		}
	}
	if config.GoBinary == "" {
		config.GoBinary, err = exec.LookPath("go")
		if err != nil {
			return nil, fmt.Errorf("find Go compiler: %w", err)
		}
	}
	if config.NodeBinary == "" {
		config.NodeBinary, _ = exec.LookPath("node")
	}
	config.GoBinary, err = exec.LookPath(config.GoBinary)
	if err != nil {
		return nil, err
	}
	config.GoBinary, err = filepath.EvalSymlinks(config.GoBinary)
	if err != nil {
		return nil, err
	}
	if config.NodeBinary != "" {
		config.NodeBinary, err = exec.LookPath(config.NodeBinary)
		if err != nil {
			return nil, err
		}
		config.NodeBinary, err = filepath.EvalSymlinks(config.NodeBinary)
		if err != nil {
			return nil, err
		}
	}
	moduleCache := filepath.Join(config.DataDir, ".toolchains", "gomodcache")
	if info, statErr := os.Stat(moduleCache); statErr != nil || !info.IsDir() {
		moduleCache = goEnvironment(config.GoBinary, "GOMODCACHE")
	}
	if config.ValidationSandbox == nil {
		config.ValidationSandbox = defaultValidationSandbox(goEnvironment(config.GoBinary, "GOROOT"), config.NodeBinary, moduleCache)
	}
	return &Manager{config: config, adapter: adapter, sandbox: config.ValidationSandbox, moduleCache: moduleCache}, nil
}

func newBuilderAdapter(config Config, adapterID string) (agent.Adapter, error) {
	switch adapterID {
	case "codex-agent":
		return agent.NewCodexAdapter(config.CodexBinary, "workspace-write")
	case "cursor-agent":
		reader, err := agent.NewCursorAdapter(config.CursorBinary)
		if err != nil {
			return nil, err
		}
		return cursorBuilder{reader: reader}, nil
	default:
		return nil, fmt.Errorf("unsupported upgrade builder adapter %q", adapterID)
	}
}

func (m *Manager) jobDir(id string) string       { return filepath.Join(m.config.DataDir, "upgrades", id) }
func (m *Manager) candidateDir(id string) string { return filepath.Join(m.jobDir(id), "candidate") }
func (m *Manager) releaseDir(id string) string   { return filepath.Join(m.jobDir(id), "release") }
func (m *Manager) backupDir(id string) string    { return filepath.Join(m.jobDir(id), "backup") }

func (m *Manager) BackupPath(id string) string { return m.backupDir(id) }

func (m *Manager) ValidationSandboxName() string { return m.sandbox.Name() }

// A sandbox executable existing on PATH does not prove that its kernel policy
// is usable (e.g. user namespaces may be disabled). Check before advertising it.
func (m *Manager) CheckValidationSandbox(ctx context.Context) error {
	parent := filepath.Join(m.config.DataDir, "upgrades")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	job, err := os.MkdirTemp(parent, "sandbox-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(job) // Only this owned, freshly allocated probe directory.
	directory := filepath.Join(job, "candidate")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := m.runCommand(ctx, directory, "/usr/bin/true")
	if err != nil {
		return fmt.Errorf("validation sandbox preflight: %w: %s", err, output)
	}
	// Check the compiler and offline dependency graph in the actual service
	// environment. A login shell's GOPATH/cache may differ from systemd's.
	manifest, err := sourceManifest(m.config.Root)
	if err != nil {
		return err
	}
	if err := copyManifest(m.config.Root, directory, manifest); err != nil {
		return err
	}
	// -m all includes upstream modules' unused tools/test dependencies. Check
	// the packages this application actually builds/tests instead; go list
	// reads source metadata but does not execute candidate code.
	for _, args := range [][]string{{m.config.GoBinary, "version"}, {m.config.GoBinary, "list", "-mod=readonly", "-deps", "-test", "./..."}} {
		output, err := m.runCommand(ctx, directory, args...)
		if err != nil {
			return fmt.Errorf("offline toolchain/cache preflight: %w: %s", err, output)
		}
	}
	return nil
}

type fileRecord struct {
	Path   string
	Digest string
	Mode   fs.FileMode
}

func sourceManifest(root string) (map[string]fileRecord, error) {
	result := map[string]fileRecord{}
	total := int64(0)
	for _, target := range sourceTargets {
		base := filepath.Join(root, target)
		info, err := os.Lstat(base)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("source symlinks are not allowed: %s", target)
		}
		walk := func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("only regular source files are allowed: %s", path)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("invalid source path %s", path)
			}
			total += info.Size()
			if total > maxSourceBytes || len(result) >= 5000 {
				return fmt.Errorf("source snapshot exceeds safety limit")
			}
			digest, err := fileSHA(path)
			if err != nil {
				return err
			}
			result[filepath.ToSlash(rel)] = fileRecord{Path: filepath.ToSlash(rel), Digest: digest, Mode: info.Mode().Perm()}
			return nil
		}
		if info.IsDir() {
			if err := filepath.WalkDir(base, walk); err != nil {
				return nil, err
			}
		} else if err := walk(base, fs.FileInfoToDirEntry(info), nil); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// validateCandidateTree makes the installable manifest exhaustive. Otherwise
// an agent could leave an executable, symlink or test fixture outside the
// source targets which is invisible to review but still available to tests.
func validateCandidateTree(root string, manifest map[string]fileRecord) error {
	entries := 0
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		entries++
		if entries > 5500 {
			return fmt.Errorf("candidate tree exceeds safety limit")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("candidate symlinks are not allowed: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("only regular candidate files are allowed: %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid candidate path %s", path)
		}
		rel = filepath.ToSlash(rel)
		if _, ok := manifest[rel]; !ok {
			return fmt.Errorf("candidate contains unsupported path: %s", rel)
		}
		if hasMultipleHardLinks(info) {
			return fmt.Errorf("candidate hard links are not allowed: %s", rel)
		}
		return nil
	})
}

func hasMultipleHardLinks(info fs.FileInfo) bool {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return false
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return false
	}
	links := value.FieldByName("Nlink")
	return links.IsValid() && links.CanUint() && links.Uint() > 1
}

func fileSHA(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func manifestSHA(files map[string]fileRecord) string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		fmt.Fprintf(h, "%s\x00%s\n", path, files[path].Digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func copyFile(source, destination string, mode fs.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return writeAtomic(destination, data, mode)
}

func writeAtomic(destination string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".upgrade-new-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(mode.Perm()); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(file.Name(), destination); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func copyManifest(source, destination string, files map[string]fileRecord) error {
	for path, record := range files {
		if err := copyFile(filepath.Join(source, filepath.FromSlash(path)), filepath.Join(destination, filepath.FromSlash(path)), record.Mode); err != nil {
			return err
		}
	}
	return nil
}

func changedFiles(before, after map[string]fileRecord) ([]model.UpgradeChange, error) {
	for path := range before {
		if _, ok := after[path]; !ok {
			return nil, fmt.Errorf("source deletion is not supported in this upgrade version: %s", path)
		}
	}
	changes := []model.UpgradeChange{}
	for path, current := range after {
		previous := before[path]
		if previous.Digest == current.Digest {
			continue
		}
		if protected(path) {
			return nil, fmt.Errorf("candidate changed protected upgrade infrastructure: %s", path)
		}
		changes = append(changes, model.UpgradeChange{Path: path, Before: previous.Digest, After: current.Digest})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	if len(changes) == 0 {
		return nil, fmt.Errorf("candidate did not change any installable source file")
	}
	if len(changes) > 200 {
		return nil, fmt.Errorf("candidate changes too many files: %d", len(changes))
	}
	return changes, nil
}

// RestartScopeForChanges is intentionally conservative. A candidate is
// control-only only when every changed path is known not to be linked into the
// long-lived execution runtime. Unknown or shared protocol paths drain and
// restart the runtime rather than risking a mixed protocol release.
func RestartScopeForChanges(changes []model.UpgradeChange) string {
	for _, change := range changes {
		path := change.Path
		controlOnly := (strings.HasPrefix(path, "internal/server/") && path != "internal/server/hub.go") ||
			strings.HasPrefix(path, "internal/tasksource/") ||
			strings.HasPrefix(path, "internal/taskaction/") ||
			strings.HasPrefix(path, "internal/router/") ||
			strings.HasPrefix(path, "internal/workflow/") ||
			strings.HasPrefix(path, "internal/rolebuilder/") ||
			strings.HasPrefix(path, "internal/concierge/") ||
			strings.HasPrefix(path, "cmd/assistantd/") ||
			strings.HasPrefix(path, "cmd/assistantctl/") ||
			strings.HasPrefix(path, "docs/") ||
			strings.HasPrefix(path, "deploy/") ||
			path == "README.md" || strings.HasSuffix(path, "-VERIFICATION.md")
		if !controlOnly {
			return model.UpgradeRestartRuntime
		}
	}
	return model.UpgradeRestartControl
}

func protected(path string) bool {
	for _, prefix := range []string{
		"cmd/assistant-supervisor/",
		"cmd/assistant-local/",
		"internal/upgrade/",
		"internal/localconfig/",
		"internal/model/upgrade.go",
		"internal/store/",
		"internal/backup/",
		"internal/runtimehost/spool.go",
		"internal/runtimehost/daemon.go",
		"internal/server/backup.go",
		"internal/server/upgrade.go",
		"internal/server/server.go",
		"internal/concierge/assistant.go",
		"go.mod", "go.sum",
	} {
		if path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

type preparationResult struct {
	Summary string `json:"summary"`
}

var preparationSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["summary"],"properties":{"summary":{"type":"string"}}}`)

func (m *Manager) Prepare(ctx context.Context, upgrade model.Upgrade) model.Upgrade {
	job := m.jobDir(upgrade.ID)
	candidate := m.candidateDir(upgrade.ID)
	release := m.releaseDir(upgrade.ID)
	if err := os.MkdirAll(job, 0o700); err != nil {
		return failed(upgrade, err)
	}
	if _, err := os.Stat(candidate); err == nil {
		stale := filepath.Join(job, fmt.Sprintf("candidate-stale-%d", time.Now().UnixMilli()))
		if err := os.Rename(candidate, stale); err != nil {
			return failed(upgrade, err)
		}
	}
	before, err := sourceManifest(m.config.Root)
	if err != nil {
		return failed(upgrade, err)
	}
	upgrade.BaseSourceSHA256 = manifestSHA(before)
	baseBinaries, err := binaryManifest(filepath.Join(m.config.Root, "bin"))
	if err != nil {
		return failed(upgrade, fmt.Errorf("read current binaries: %w", err))
	}
	upgrade.BaseBinaries = map[string]string{}
	for name, record := range baseBinaries {
		if name != "assistant-supervisor" {
			upgrade.BaseBinaries[name] = record.Digest
		}
	}
	upgrade.BaseRelease = upgrade.BaseBinaries["assistant-local"]
	if upgrade.BaseRelease == "" {
		return failed(upgrade, fmt.Errorf("read current release: assistant-local is missing"))
	}
	if err = os.MkdirAll(candidate, 0o700); err != nil {
		return failed(upgrade, err)
	}
	if err = copyManifest(m.config.Root, candidate, before); err != nil {
		return failed(upgrade, err)
	}
	prompt := `你正在为 Work Assistant 制作一个候选升级。只修改当前隔离副本中的源码；不得访问或修改运行中的系统、外部仓库、数据库、认证信息或网络服务。
实现下面的用户需求并补充必要测试。不要更改 go.mod/go.sum、控制端或 Runtime 数据库 schema/migration、首页升级动作协议、升级 API、升级构建器、assistant-supervisor、assistant-local 的进程握手或启动脚本；第一版升级器会拒绝这些改动。不要删除现有文件。普通工作仍需遵守现有人工确认、Session 归属和只读边界。
完成后运行与你的改动直接相关的测试，并用 JSON 简洁总结实际修改；最终是否可安装由外部验证器和用户决定。

升级需求：
` + upgrade.Instructions
	var eventLog strings.Builder
	var eventMu sync.Mutex
	modelID, agentID := m.config.ModelID, ""
	execution := model.ExecutionSettings{ReasoningEffortSource: "runtime_default"}
	builder := m.adapter
	if upgrade.Builder != nil {
		if m.config.RuntimeID == "" || upgrade.Builder.RuntimeID != m.config.RuntimeID {
			return failed(upgrade, fmt.Errorf("configured upgrade builder is not supported by this local supervisor"))
		}
		if upgrade.Builder.AdapterID != m.config.AdapterID {
			builder, err = newBuilderAdapter(m.config, upgrade.Builder.AdapterID)
			if err != nil {
				return failed(upgrade, err)
			}
		}
		modelID, agentID = upgrade.Builder.ModelID, upgrade.Builder.ID
		execution.ReasoningEffort = upgrade.Builder.ReasoningEffort
		if execution.ReasoningEffort != "" {
			execution.ReasoningEffortSource = "member_default"
		}
		prompt = upgrade.Builder.Role.ExecutionInstructions() + "\n\n" + prompt
	}
	result := builder.Run(ctx, model.RunSpec{ExecutionSettings: execution, AgentID: agentID, TaskTitle: upgrade.Title, TaskGoal: upgrade.Instructions, Instructions: prompt, ModelID: modelID, OutputSchema: preparationSchema}, candidate, nil, func(event agent.Event) {
		eventMu.Lock()
		defer eventMu.Unlock()
		if event.Execution != nil {
			upgrade.ExecutionSettings = *event.Execution
		}
		if event.AgentSessionRef != "" {
			upgrade.SessionRef = event.AgentSessionRef
		}
		if event.Message != "" {
			if eventLog.Len() < 30000 {
				fmt.Fprintf(&eventLog, "%s: %s\n", event.Stream, clipped(event.Message, 4000))
			}
		}
		if event.Error != "" {
			if eventLog.Len() < 30000 {
				fmt.Fprintf(&eventLog, "error: %s\n", clipped(event.Error, 4000))
			}
		}
	})
	upgrade.Log = clipped(eventLog.String(), 30000)
	if result.Err != nil {
		return failed(upgrade, fmt.Errorf("upgrade agent: %w", result.Err))
	}
	var answer preparationResult
	if err := json.Unmarshal([]byte(result.Output), &answer); err != nil || strings.TrimSpace(answer.Summary) == "" {
		return failed(upgrade, fmt.Errorf("upgrade agent returned invalid summary: %w", err))
	}
	beforeValidation, err := sourceManifest(candidate)
	if err != nil {
		return failed(upgrade, err)
	}
	if err = validateCandidateTree(candidate, beforeValidation); err != nil {
		return failed(upgrade, err)
	}
	initialChanges, err := changedFiles(before, beforeValidation)
	if err != nil {
		return failed(upgrade, err)
	}
	if m.config.NodeBinary == "" {
		for _, change := range initialChanges {
			if strings.EqualFold(filepath.Ext(change.Path), ".js") {
				return failed(upgrade, fmt.Errorf("Node.js is required to validate JavaScript change %s", change.Path))
			}
		}
	}
	upgrade.Summary = strings.TrimSpace(answer.Summary)

	if err := os.MkdirAll(filepath.Join(release, "bin"), 0o700); err != nil {
		return failed(upgrade, err)
	}
	for _, command := range [][]string{{m.config.GoBinary, "test", "./..."}, {m.config.GoBinary, "vet", "./..."}} {
		output, err := m.runCommand(ctx, candidate, command...)
		upgrade.Log += output
		if err != nil {
			return failed(upgrade, err)
		}
	}
	if m.config.NodeBinary != "" {
		javascript, _ := filepath.Glob(filepath.Join(candidate, "internal", "server", "ui", "*.js"))
		sort.Strings(javascript)
		for _, file := range javascript {
			output, err := m.runCommand(ctx, candidate, m.config.NodeBinary, "--check", file)
			upgrade.Log += output
			if err != nil {
				return failed(upgrade, err)
			}
		}
		javascriptTests, _ := filepath.Glob(filepath.Join(candidate, "internal", "server", "ui", "*.test.cjs"))
		if len(javascriptTests) > 0 {
			args := append([]string{m.config.NodeBinary, "--test"}, javascriptTests...)
			output, err := m.runCommand(ctx, candidate, args...)
			upgrade.Log += output
			if err != nil {
				return failed(upgrade, err)
			}
		}
	}
	// Candidates are content-hashed snapshots, not Git checkouts. Do not inherit
	// an unrelated ancestor repository (or its broken metadata) as build provenance.
	output, err := m.runCommand(ctx, candidate, m.config.GoBinary, "build", "-buildvcs=false", "-o", filepath.Join(release, "bin")+string(filepath.Separator), "./cmd/...")
	upgrade.Log += output
	if err != nil {
		return failed(upgrade, err)
	}
	after, err := sourceManifest(candidate)
	if err != nil {
		return failed(upgrade, err)
	}
	if err = validateCandidateTree(candidate, after); err != nil {
		return failed(upgrade, err)
	}
	if manifestSHA(after) != manifestSHA(beforeValidation) {
		return failed(upgrade, fmt.Errorf("candidate source changed while validation commands were running"))
	}
	changes, err := changedFiles(before, after)
	if err != nil {
		return failed(upgrade, err)
	}
	upgrade.Changes = changes
	upgrade.RestartScope = RestartScopeForChanges(changes)
	upgrade.Patch, err = reviewPatch(m.config.Root, candidate, changes)
	if err != nil {
		return failed(upgrade, err)
	}
	upgrade.SourceSHA256 = manifestSHA(after)
	digest, err := m.candidateDigest(upgrade.ID, upgrade.SourceSHA256)
	if err != nil {
		return failed(upgrade, err)
	}
	upgrade.CandidateSHA256 = digest
	upgrade.State = "READY"
	upgrade.Error = ""
	upgrade.Log = clipped(upgrade.Log, 60000)
	return upgrade
}

func (m *Manager) runCommand(ctx context.Context, directory string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("validation command is empty")
	}
	job := filepath.Dir(directory)
	for _, name := range []string{"validator-home", "validator-tmp", "go-build-cache", "validator-cache", "validator-config"} {
		if err := os.MkdirAll(filepath.Join(job, name), 0o700); err != nil {
			return "", err
		}
	}
	wrapped, err := m.sandbox.Wrap(job, args)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	command.Dir = directory
	command.Env = m.validationEnvironment(job)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 2 * time.Second
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err = command.Start()
	if err == nil {
		processGroup := command.Process.Pid
		err = command.Wait()
		// Validation code must not leave a helper behind to mutate the reviewed
		// candidate or release after the command appears to have completed.
		_ = syscall.Kill(-processGroup, syscall.SIGKILL)
	}
	entry := fmt.Sprintf("\n$ [%s] %s\n%s", m.sandbox.Name(), strings.Join(args, " "), clipped(output.String(), 12000))
	if err != nil {
		return entry, fmt.Errorf("validation command failed (%s): %w", strings.Join(args, " "), err)
	}
	return entry, nil
}

func (m *Manager) validationEnvironment(job string) []string {
	pathParts := []string{filepath.Dir(m.config.GoBinary), "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if m.config.NodeBinary != "" {
		pathParts = append([]string{filepath.Dir(m.config.NodeBinary)}, pathParts...)
	}
	environment := []string{
		"PATH=" + strings.Join(uniqueStrings(pathParts), string(os.PathListSeparator)),
		"HOME=" + filepath.Join(job, "validator-home"),
		"TMPDIR=" + filepath.Join(job, "validator-tmp"),
		"XDG_CACHE_HOME=" + filepath.Join(job, "validator-cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(job, "validator-config"),
		"LC_ALL=C",
		"TERM=dumb",
		"WORK_ASSISTANT_VALIDATION_SANDBOX=" + m.sandbox.Name(),
		"CGO_ENABLED=0",
		"GOENV=off",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTELEMETRY=off",
		"GOTOOLCHAIN=local",
		"GOCACHE=" + filepath.Join(job, "go-build-cache"),
		"GOTMPDIR=" + filepath.Join(job, "validator-tmp"),
	}
	if m.moduleCache != "" {
		environment = append(environment, "GOMODCACHE="+m.moduleCache)
	}
	return environment
}

func goEnvironment(goBinary, name string) string {
	command := exec.Command(goBinary, "env", name)
	// Pin the selected executable's own toolchain, not an older GOROOT exported
	// by the user's shell. Candidate commands get the same clean policy.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GOROOT=") && !strings.HasPrefix(value, "GOTOOLCHAIN=") && !strings.HasPrefix(value, "GOENV=") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "GOTOOLCHAIN=local", "GOENV=off")
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func (m *Manager) candidateDigest(id, sourceDigest string) (string, error) {
	files, err := binaryManifest(filepath.Join(m.releaseDir(id), "bin"))
	if err != nil {
		return "", err
	}
	if _, ok := files["assistant-local"]; !ok {
		return "", fmt.Errorf("candidate is missing assistant-local")
	}
	h := sha256.New()
	fmt.Fprintln(h, sourceDigest)
	paths := make([]string, 0, len(files))
	for path := range files {
		if path != "assistant-supervisor" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		fmt.Fprintf(h, "%s\x00%s\n", path, files[path].Digest)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func binaryManifest(directory string) (map[string]fileRecord, error) {
	result := map[string]fileRecord{}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("invalid release entry %s", entry.Name())
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("invalid release file %s", entry.Name())
		}
		digest, err := fileSHA(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		result[entry.Name()] = fileRecord{Path: entry.Name(), Digest: digest, Mode: info.Mode().Perm()}
	}
	return result, nil
}

func failed(upgrade model.Upgrade, err error) model.Upgrade {
	upgrade.State = "FAILED"
	upgrade.Error = err.Error()
	return upgrade
}

func clipped(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return "…\n" + string(runes[len(runes)-limit:])
}

// reviewPatch records every replaced text segment. A candidate whose review
// material would need truncation is rejected rather than asking for blind trust.
func reviewPatch(beforeRoot, afterRoot string, changes []model.UpgradeChange) (string, error) {
	var patch strings.Builder
	for _, change := range changes {
		var before []byte
		var err error
		if change.Before != "" {
			before, err = os.ReadFile(filepath.Join(beforeRoot, filepath.FromSlash(change.Path)))
			if err != nil {
				return "", err
			}
		}
		after, err := os.ReadFile(filepath.Join(afterRoot, filepath.FromSlash(change.Path)))
		if err != nil {
			return "", err
		}
		if !utf8.Valid(before) || !utf8.Valid(after) || strings.IndexByte(string(before), 0) >= 0 || strings.IndexByte(string(after), 0) >= 0 {
			return "", fmt.Errorf("binary source changes are not supported: %s", change.Path)
		}
		var oldLines, newLines []string
		if len(before) > 0 {
			oldLines = strings.Split(string(before), "\n")
		}
		if len(after) > 0 {
			newLines = strings.Split(string(after), "\n")
		}
		prefix := 0
		for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
			prefix++
		}
		suffix := 0
		for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
			suffix++
		}
		fmt.Fprintf(&patch, "--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", change.Path, change.Path, prefix+1, len(oldLines)-prefix-suffix, prefix+1, len(newLines)-prefix-suffix)
		for _, line := range oldLines[prefix : len(oldLines)-suffix] {
			fmt.Fprintf(&patch, "-%s\n", line)
		}
		for _, line := range newLines[prefix : len(newLines)-suffix] {
			fmt.Fprintf(&patch, "+%s\n", line)
		}
		if patch.Len() > 120000 {
			return "", fmt.Errorf("candidate review patch exceeds 120 KB")
		}
	}
	return patch.String(), nil
}

// VerifyCandidate binds installation to the exact source and binaries a human
// reviewed. It also refuses installation over an independently changed release.
func (m *Manager) VerifyCandidate(upgrade model.Upgrade) error {
	if upgrade.RestartScope != "" && upgrade.RestartScope != RestartScopeForChanges(upgrade.Changes) {
		return fmt.Errorf("candidate restart scope does not match its reviewed changes")
	}
	files, err := sourceManifest(m.candidateDir(upgrade.ID))
	if err != nil {
		return err
	}
	if manifestSHA(files) != upgrade.SourceSHA256 {
		return fmt.Errorf("candidate source changed after review")
	}
	digest, err := m.candidateDigest(upgrade.ID, upgrade.SourceSHA256)
	if err != nil {
		return err
	}
	if digest != upgrade.CandidateSHA256 {
		return fmt.Errorf("candidate binaries changed after review")
	}
	liveFiles, err := sourceManifest(m.config.Root)
	if err != nil {
		return err
	}
	changes := make(map[string]model.UpgradeChange, len(upgrade.Changes))
	for _, change := range upgrade.Changes {
		changes[change.Path] = change
	}
	for path := range liveFiles {
		if _, ok := files[path]; !ok {
			return fmt.Errorf("live source changed after candidate preparation: %s", path)
		}
	}
	for path, expected := range files {
		live, exists := liveFiles[path]
		change, changed := changes[path]
		if !changed {
			if !exists || live.Digest != expected.Digest {
				return fmt.Errorf("live source changed after candidate preparation: %s", path)
			}
			continue
		}
		if !exists {
			if change.Before == "" {
				continue
			}
			return fmt.Errorf("live source is missing: %s", path)
		}
		if live.Digest != change.Before && live.Digest != change.After {
			return fmt.Errorf("live source changed after candidate preparation: %s", path)
		}
	}
	candidateBinaries, err := binaryManifest(filepath.Join(m.releaseDir(upgrade.ID), "bin"))
	if err != nil {
		return err
	}
	liveBinaries, err := binaryManifest(filepath.Join(m.config.Root, "bin"))
	if err != nil {
		return err
	}
	for name, candidate := range candidateBinaries {
		if name == "assistant-supervisor" {
			continue
		}
		live, exists := liveBinaries[name]
		base := upgrade.BaseBinaries[name]
		if base == "" && name == "assistant-local" {
			base = upgrade.BaseRelease
		}
		if !exists {
			if base == "" {
				continue
			}
			return fmt.Errorf("live binary is missing: %s", name)
		}
		if live.Digest != candidate.Digest && live.Digest != base {
			return fmt.Errorf("live binary changed after candidate preparation: %s", name)
		}
	}
	return nil
}

func (m *Manager) IsApplied(upgrade model.Upgrade) bool {
	files, err := sourceManifest(m.config.Root)
	if err != nil || manifestSHA(files) != upgrade.SourceSHA256 {
		return false
	}
	binaries, err := binaryManifest(filepath.Join(m.releaseDir(upgrade.ID), "bin"))
	if err != nil {
		return false
	}
	for name, expected := range binaries {
		if name == "assistant-supervisor" {
			continue
		}
		digest, err := fileSHA(filepath.Join(m.config.Root, "bin", name))
		if err != nil || digest != expected.Digest {
			return false
		}
	}
	return true
}

func (m *Manager) Backup(ctx context.Context, state *store.Store, upgrade model.Upgrade) (string, error) {
	backup := m.backupDir(upgrade.ID)
	if _, err := os.Stat(filepath.Join(backup, "control.sqlite")); err == nil {
		if _, err := os.Stat(filepath.Join(m.config.DataDir, "runtime.sqlite")); err == nil {
			if err := protection.VerifySQLite(ctx, filepath.Join(backup, "runtime.sqlite")); err != nil {
				return "", err
			}
		}
		return backup, protection.VerifySQLite(ctx, filepath.Join(backup, "control.sqlite"))
	}
	if err := os.MkdirAll(filepath.Join(backup, "source"), 0o700); err != nil {
		return "", err
	}
	manifest, err := sourceManifest(m.config.Root)
	if err != nil {
		return "", err
	}
	if err := copyManifest(m.config.Root, filepath.Join(backup, "source"), manifest); err != nil {
		return "", err
	}
	binaries, err := binaryManifest(filepath.Join(m.config.Root, "bin"))
	if err != nil {
		return "", err
	}
	for name, record := range binaries {
		if name == "assistant-supervisor" {
			continue
		}
		if err := copyFile(filepath.Join(m.config.Root, "bin", name), filepath.Join(backup, "bin", name), record.Mode); err != nil {
			return "", err
		}
	}
	// Save the runtime spool too; control.sqlite is the last, durable completion
	// marker. Never overwrite either live database during a code rollback.
	runtimeDB := filepath.Join(m.config.DataDir, "runtime.sqlite")
	if _, err := os.Stat(runtimeDB); err == nil {
		destination := filepath.Join(backup, "runtime.sqlite")
		if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
			if err := (protection.DatabaseSource{Path: runtimeDB}).Backup(ctx, destination); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		if err := protection.VerifySQLite(ctx, destination); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	database := filepath.Join(backup, "control.sqlite")
	if err := state.Backup(ctx, database); err != nil {
		return "", err
	}
	return backup, nil
}

func (m *Manager) Apply(upgrade model.Upgrade) error {
	if err := m.VerifyCandidate(upgrade); err != nil {
		return err
	}
	candidate := m.candidateDir(upgrade.ID)
	for _, change := range upgrade.Changes {
		record, err := os.Stat(filepath.Join(candidate, filepath.FromSlash(change.Path)))
		if err != nil || !record.Mode().IsRegular() {
			return fmt.Errorf("candidate file unavailable: %s", change.Path)
		}
		if err := copyFile(filepath.Join(candidate, filepath.FromSlash(change.Path)), filepath.Join(m.config.Root, filepath.FromSlash(change.Path)), record.Mode()); err != nil {
			return err
		}
	}
	binaries, err := binaryManifest(filepath.Join(m.releaseDir(upgrade.ID), "bin"))
	if err != nil {
		return err
	}
	for name, record := range binaries {
		if name == "assistant-supervisor" {
			continue
		}
		if err := copyFile(filepath.Join(m.releaseDir(upgrade.ID), "bin", name), filepath.Join(m.config.Root, "bin", name), record.Mode); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Rollback(upgrade model.Upgrade) error {
	backup := m.backupDir(upgrade.ID)
	for _, change := range upgrade.Changes {
		destination := filepath.Join(m.config.Root, filepath.FromSlash(change.Path))
		if change.Before == "" {
			if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		source := filepath.Join(backup, "source", filepath.FromSlash(change.Path))
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if err := copyFile(source, destination, info.Mode()); err != nil {
			return err
		}
	}
	candidateBinaries, err := binaryManifest(filepath.Join(m.releaseDir(upgrade.ID), "bin"))
	if err != nil {
		return err
	}
	binaries, err := binaryManifest(filepath.Join(backup, "bin"))
	if err != nil {
		return err
	}
	for name := range candidateBinaries {
		if name == "assistant-supervisor" {
			continue
		}
		if _, existed := binaries[name]; !existed {
			if err := os.Remove(filepath.Join(m.config.Root, "bin", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	for name, record := range binaries {
		if err := copyFile(filepath.Join(backup, "bin", name), filepath.Join(m.config.Root, "bin", name), record.Mode); err != nil {
			return err
		}
	}
	return nil
}
