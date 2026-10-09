package runtimehost

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

//go:embed windows_phase0.ps1
var windowsPhase0Script string

type windowsProfile struct {
	Autonomous             bool   `json:"autonomous"`
	RepositoryRoot         string `json:"repository_root"`
	BuildContract          string `json:"build_contract"`
	BuildScriptSHA256      string `json:"build_script_sha256"`
	ProductCompileCommands string `json:"product_compile_commands"`
	WinRMCommand           string `json:"winrm_command"`
	CMake                  string `json:"cmake"`
	Clang                  string `json:"clang"`
	Ninja                  string `json:"ninja"`
	VSDevCmd               string `json:"vs_dev_cmd"`
	SQLiteInclude          string `json:"sqlite_include"`
	SQLiteLibrary          string `json:"sqlite_library"`
	SQLiteHeaderSHA256     string `json:"sqlite_header_sha256"`
	SQLiteLibrarySHA256    string `json:"sqlite_library_sha256"`
	SQLiteDLLSHA256        string `json:"sqlite_dll_sha256"`
	AllowPolicySwitch      bool   `json:"allow_policy_switch"`
	MinimumFreeGB          int    `json:"minimum_free_gb,omitempty"`
}
type windowsFile struct {
	Name string `json:"name"`
	Data string `json:"data"`
}
type windowsPayload struct {
	RepositoryArchive []byte         `json:"repository_archive,omitempty"`
	RepositorySHA256  string         `json:"repository_sha256,omitempty"`
	ParentTaskID      string         `json:"parent_task_id"`
	JobID             string         `json:"job_id"`
	Mode              string         `json:"mode"`
	SnapshotSHA256    string         `json:"snapshot_sha256"`
	Profile           windowsProfile `json:"profile"`
	Files             []windowsFile  `json:"files"`
}
type windowsReceipt struct {
	State  string                   `json:"state"`
	Result *model.EnvironmentResult `json:"result,omitempty"`
}

var phase0Files = []string{"CMakeLists.txt", "path_fixture.h", "path_fixture_test.cpp", "phase0.manifest", "README.md", "sqlite_path_probe.cpp"}

const windowsEnvironmentHealthTTL = 5 * time.Minute

type windowsHealthCacheEntry struct {
	fingerprint string
	capability  model.ExecutionCapability
	expiresAt   time.Time
}

// Advertise configured host capabilities.  Autonomous Windows execution is a
// product environment, rather than a best-effort shell command: when a
// runtime connects, run a small, read-only preflight and surface its result to
// the Manager.  We deliberately do not probe compilers here; repositories own
// their build contract and must use their official build script.
func (d *Daemon) executionCapabilities() map[string]model.ExecutionCapability {
	cap := model.ExecutionCapability{Reason: "缺少受控 Windows profile、固定依赖校验或宿主策略切换授权"}
	raw, err := os.ReadFile(filepath.Join(d.config.WorkRoot, "windows-profiles.json"))
	var profiles map[string]windowsProfile
	if err == nil && json.Unmarshal(raw, &profiles) == nil {
		p, ok := profiles["windows_seekdb_phase0"]
		st, e := os.Stat(p.WinRMCommand)
		if ok && e == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 && filepath.IsAbs(p.WinRMCommand) && p.AllowPolicySwitch && len(p.SQLiteHeaderSHA256) == 64 && len(p.SQLiteLibrarySHA256) == 64 && len(p.SQLiteDLLSHA256) == 64 {
			cap = model.ExecutionCapability{Available: true}
		}
		if ok && p.Autonomous && e == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 && filepath.IsAbs(p.WinRMCommand) {
			vm := d.autonomousWindowsCapability(p)
			return map[string]model.ExecutionCapability{
				"windows_vm":            vm,
				"windows_seekdb_phase0": {Reason: "此 VM 已改为 Agent 自主远程执行/上传工具，不再使用专用 environment_request。请在原开发 Session 使用本轮提供的 client.py。"},
			}
		}
	}
	return map[string]model.ExecutionCapability{"windows_seekdb_phase0": cap}
}

func windowsProfileFingerprint(p windowsProfile) string {
	// Exclude all legacy build settings.  They do not define whether the VM is
	// usable, and letting them affect this cache would turn normal repository
	// build changes into accidental environment changes.
	raw, _ := json.Marshal(struct {
		Autonomous    bool   `json:"autonomous"`
		WinRMCommand  string `json:"winrm_command"`
		MinimumFreeGB int    `json:"minimum_free_gb"`
	}{p.Autonomous, p.WinRMCommand, p.MinimumFreeGB})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (d *Daemon) cachedAutonomousWindowsCapability(p windowsProfile) (model.ExecutionCapability, bool) {
	fingerprint := windowsProfileFingerprint(p)
	d.environmentMu.Lock()
	defer d.environmentMu.Unlock()
	entry, ok := d.environmentHealth["windows_vm"]
	if !ok || entry.fingerprint != fingerprint || time.Now().After(entry.expiresAt) {
		return model.ExecutionCapability{}, false
	}
	return entry.capability, true
}

func (d *Daemon) autonomousWindowsCapability(p windowsProfile) model.ExecutionCapability {
	if cached, ok := d.cachedAutonomousWindowsCapability(p); ok {
		return cached
	}

	checkedAt := time.Now()
	capability := model.ExecutionCapability{CheckedAtMS: checkedAt.UnixMilli(), Fingerprint: windowsProfileFingerprint(p)}
	if !p.Autonomous {
		capability.Reason = "Windows VM 未启用自主执行"
		return d.cacheAutonomousWindowsCapability(p, capability, checkedAt)
	}
	st, err := os.Stat(p.WinRMCommand)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 || !filepath.IsAbs(p.WinRMCommand) {
		capability.Reason = "Windows WinRM 传输程序不可用；请检查受控运行环境配置"
		return d.cacheAutonomousWindowsCapability(p, capability, checkedAt)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	// This script is fixed, read-only, and intentionally does not carry model
	// text or repository paths.  It verifies transport, PowerShell, and enough
	// free space to start a build without trying to infer toolchains.
	const script = "$ErrorActionPreference = 'Stop'\n$drive = Get-PSDrive -Name C\n$freeGB = [math]::Floor([double]$drive.Free / 1GB)\n[Console]::Out.WriteLine('WORK_ASSISTANT_VM_READY')\n[Console]::Out.WriteLine('WORK_ASSISTANT_FREE_GB=' + $freeGB)\n"
	cmd := exec.CommandContext(ctx, p.WinRMCommand)
	cmd.Stdin = strings.NewReader(script)
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		capability.Reason = "Windows VM 环境预检超时；请检查 VM 或 WinRM 连接"
	} else if runErr != nil {
		capability.Reason = "Windows VM 环境预检失败；请检查 VM 是否运行以及 WinRM 是否可用"
	} else {
		freeGB, parsed := windowsFreeGB(string(output))
		minimum := p.MinimumFreeGB
		if minimum <= 0 {
			minimum = 12
		}
		switch {
		case !strings.Contains(string(output), "WORK_ASSISTANT_VM_READY") || !parsed:
			capability.Reason = "Windows VM 环境预检没有返回完整结果；请检查 WinRM 传输程序"
		case freeGB < int64(minimum):
			capability.Reason = fmt.Sprintf("Windows VM 可用磁盘仅 %d GiB，低于可靠构建所需的 %d GiB", freeGB, minimum)
		default:
			capability.Available = true
		}
	}
	return d.cacheAutonomousWindowsCapability(p, capability, checkedAt)
}

func (d *Daemon) cacheAutonomousWindowsCapability(p windowsProfile, capability model.ExecutionCapability, checkedAt time.Time) model.ExecutionCapability {
	d.environmentMu.Lock()
	defer d.environmentMu.Unlock()
	if d.environmentHealth == nil {
		d.environmentHealth = make(map[string]windowsHealthCacheEntry)
	}
	d.environmentHealth["windows_vm"] = windowsHealthCacheEntry{
		fingerprint: windowsProfileFingerprint(p), capability: capability, expiresAt: checkedAt.Add(windowsEnvironmentHealthTTL),
	}
	return capability
}

func windowsFreeGB(output string) (int64, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "WORK_ASSISTANT_FREE_GB=") {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "WORK_ASSISTANT_FREE_GB=")), 10, 64)
		return value, err == nil && value >= 0
	}
	return 0, false
}

func snapshotWindowsFiles(directory string) ([]windowsFile, string, error) {
	real, err := filepath.EvalSymlinks(directory)
	if err != nil || real != directory {
		return nil, "", errors.New("Phase 0 source directory must exist and not be symlinked")
	}
	var files []windowsFile
	digest := sha256.New()
	total := 0
	for _, name := range phase0Files {
		p := filepath.Join(directory, name)
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() > 128*1024 {
			return nil, "", fmt.Errorf("missing, symlinked or oversized Phase 0 file: %s", name)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, "", err
		}
		total += len(data)
		if total > 512*1024 {
			return nil, "", errors.New("Phase 0 snapshot exceeds 512 KiB")
		}
		fmt.Fprintf(digest, "%s\x00%d\x00", name, len(data))
		digest.Write(data)
		files = append(files, windowsFile{Name: name, Data: base64.StdEncoding.EncodeToString(data)})
	}
	return files, hex.EncodeToString(digest.Sum(nil)), nil
}

type boundedWindowsLog struct{ bytes.Buffer }

func (b *boundedWindowsLog) Write(p []byte) (int, error) {
	n := len(p)
	const limit = 96 * 1024
	if len(p) >= limit {
		b.Reset()
		p = p[len(p)-limit:]
	} else if b.Len()+len(p) > limit {
		b.Next(b.Len() + len(p) - limit)
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func callWindows(ctx context.Context, p windowsPayload) (string, error) {
	// PowerShell receives only base64 JSON; no model text is interpolated as code.
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	script := strings.Replace(windowsPhase0Script, "__PAYLOAD_BASE64__", base64.StdEncoding.EncodeToString(raw), 1)
	cmd := exec.CommandContext(ctx, p.Profile.WinRMCommand)
	cmd.Stdin = strings.NewReader(script)
	var log boundedWindowsLog
	cmd.Stdout = &log
	cmd.Stderr = &log
	err = cmd.Run()
	return log.String(), err
}

func decodeWindowsResult(log string) (*model.EnvironmentResult, error) {
	const marker = "WORK_ASSISTANT_RESULT="
	at := strings.LastIndex(log, marker)
	if at < 0 {
		return nil, errors.New("Windows result not received; execution may be uncertain")
	}
	lines := strings.SplitN(log[at+len(marker):], "\n", 2)
	line := lines[0]
	var result model.EnvironmentResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &result); err != nil {
		return nil, errors.New("invalid Windows result envelope")
	}
	if result.Status != "passed" && result.Status != "failed" && result.Status != "unavailable" {
		return nil, errors.New("invalid Windows verdict")
	}
	result.Log = log[:at]
	// WinRM may deliver stderr after the stdout result envelope.
	if len(lines) == 2 && strings.TrimSpace(lines[1]) != "" {
		result.Log += "\n[transport stderr/tail]\n" + lines[1]
	}
	if len(result.Log) > 24000 {
		result.Log = result.Log[len(result.Log)-24000:]
	}
	return &result, nil
}

func (d *Daemon) executeEnvironment(ctx context.Context, spec model.RunSpec, emit func(agent.Event)) agent.Result {
	env := spec.Environment
	answer := model.EnvironmentResult{Status: "unavailable", Profile: "windows_seekdb_phase0"}
	finish := func(message string) agent.Result {
		if message != "" {
			answer.Status = "unavailable"
			answer.Message = message
		}
		report, _ := json.MarshalIndent(answer, "", "  ")
		out, _ := json.Marshal(workflow.Result{Outcome: "review", Message: answer.Message, Artifacts: []workflow.File{{Name: "windows-execution.json", Content: string(report)}}, EnvironmentResult: &answer})
		return agent.Result{Output: string(out), ExitCode: 0}
	}
	if env == nil || !spec.ReadOnly || spec.ExecutionGrant != nil || env.Profile != "windows_seekdb_phase0" || env.Grant.Repository != "oceanbase/seekdb" || len(env.Grant.PlanHash) != 64 || env.Grant.ReviewID == "" || !managedID.MatchString(env.SourceSessionID) || !managedID.MatchString(env.ParentTaskID) || !managedID.MatchString(spec.TaskID) {
		return finish("无效的受控 Windows 执行授权。")
	}
	raw, err := os.ReadFile(filepath.Join(d.config.WorkRoot, "windows-profiles.json"))
	if err != nil {
		return finish("主机尚未配置 windows-profiles.json；需要受控 Windows 入口和固定工具链。")
	}
	var profiles map[string]windowsProfile
	if json.Unmarshal(raw, &profiles) != nil {
		return finish("Windows 主机配置格式无效。")
	}
	profile, ok := profiles[env.Profile]
	if ok && profile.Autonomous {
		return finish("Windows 已启用自主 exec/upload 工具，请回原开发 Session 使用通用工具；不再调用专用探针执行器。")
	}
	if !ok || !filepath.IsAbs(profile.WinRMCommand) || len(profile.SQLiteHeaderSHA256) != 64 || len(profile.SQLiteLibrarySHA256) != 64 || len(profile.SQLiteDLLSHA256) != 64 {
		return finish("Windows profile 缺少入口或固定依赖校验值。")
	}
	parentReceipt := filepath.Join(d.config.WorkRoot, "development", env.SourceSessionID, "workspace.json")
	raw, err = os.ReadFile(parentReceipt)
	if err != nil {
		return finish("缺少原任务已登记的隔离工作区。")
	}
	var w developmentWorkspace
	if json.Unmarshal(raw, &w) != nil || w.Repository != env.Grant.Repository || w.BaseBranch != env.Grant.BaseBranch || w.Directory != filepath.Join(d.config.WorkRoot, "sessions", env.SourceSessionID, "repository") {
		return finish("原任务工作区与审批范围不一致。")
	}
	if w.BaseDirectory != "" {
		if err = validateDevelopmentWorktree(ctx, w); err != nil {
			return finish("工作区完整性检查失败：" + err.Error())
		}
	}
	// Serialize on this runtime; the Windows-side lock also protects other runtimes.
	d.preparationMu.Lock()
	if d.windowsGate == nil {
		d.windowsGate = make(chan struct{}, 1)
	}
	gate := d.windowsGate
	d.preparationMu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return finish("等待 Windows 执行槽位时已取消。")
	}
	record := filepath.Join(d.config.WorkRoot, "windows-jobs", spec.TaskID)
	if err = os.MkdirAll(record, 0700); err != nil {
		return finish("无法创建持久化执行目录。")
	}
	receipt := filepath.Join(record, "receipt.json")
	if previous, e := os.ReadFile(receipt); e == nil {
		var state windowsReceipt
		if json.Unmarshal(previous, &state) != nil {
			return finish("Windows 执行回执损坏；停止重复执行。")
		}
		if state.Result != nil {
			answer = *state.Result
			return finish("")
		}
		return finish("此前 Windows 执行结果不确定；不会盲目重复执行。新任务会先取得 Windows 串行锁并恢复遗留策略。")
	} else if !errors.Is(e, os.ErrNotExist) {
		return finish("无法读取此前 Windows 执行回执，停止重复执行。")
	}
	files, hash, err := snapshotWindowsFiles(filepath.Join(w.Directory, "tools", "windows", "long_path_phase0"))
	if err != nil {
		return finish(err.Error())
	}
	answer.SnapshotSHA256 = hash
	payload := windowsPayload{ParentTaskID: env.ParentTaskID, JobID: spec.TaskID, Mode: "preflight", SnapshotSHA256: hash, Profile: profile, Files: files}
	archive, repositoryHash, err := snapshotRepository(ctx, w.Directory)
	if err != nil {
		return finish(err.Error())
	}
	if err = os.WriteFile(filepath.Join(record, "repository.tar.gz"), archive, 0600); err != nil {
		return finish("无法持久化源码快照")
	}
	payload.RepositorySHA256 = repositoryHash
	if err = durableWorkspaceJSON(filepath.Join(record, "snapshot.json"), payload); err != nil {
		return finish("源码快照持久化失败。")
	}
	emit(agent.Event{Message: "Windows 执行器：检查受控通道、工具链和固定 SQLite 依赖。"})
	// Only the read-only preflight is retried. Never blindly retry a remote build/test.
	for attempt := 0; attempt < 2; attempt++ {
		preCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		log, e := callWindows(preCtx, payload)
		cancel()
		pre, parseErr := decodeWindowsResult(log)
		if e == nil && parseErr == nil {
			if pre.Status != "passed" {
				answer = *pre
				answer.SnapshotSHA256 = hash
				return finish("")
			}
			err = nil
			break
		}
		err = errors.New("Windows 连接预检失败；没有执行测试或切换策略")
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return finish(err.Error())
	}
	if err = durableWorkspaceJSON(receipt, windowsReceipt{State: "SUBMITTING"}); err != nil {
		return finish("无法持久化执行意图，未提交 Windows 测试。")
	}
	emit(agent.Event{Message: "Windows 执行器：当前工作树快照已冻结，通过仓库 build.ps1 进行 LongPathsEnabled=0/1 验证，完成后恢复原值。"})
	payload.Mode = "execute"
	payload.RepositoryArchive = archive
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	log, runErr := callWindows(runCtx, payload)
	cancel()
	if err = os.WriteFile(filepath.Join(record, "execution.log"), []byte(log), 0600); err != nil {
		return finish("执行日志持久化失败；保留不确定回执，禁止自动重复执行。")
	}
	result, parseErr := decodeWindowsResult(log)
	if runErr != nil || parseErr != nil {
		return finish("Windows 执行连接中断或结果不完整；保留现场。不得视为通过；后续执行须先确认策略恢复。")
	}
	answer = *result
	answer.Profile = env.Profile
	answer.SnapshotSHA256 = hash
	if err = durableWorkspaceJSON(receipt, windowsReceipt{State: "COMPLETED", Result: &answer}); err != nil {
		return finish("Windows 结果回执持久化失败，保留现场供核对。")
	}
	return finish("")
}
