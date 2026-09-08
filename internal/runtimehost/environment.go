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
	"strings"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

//go:embed windows_phase0.ps1
var windowsPhase0Script string

type windowsProfile struct {
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
}
type windowsFile struct {
	Name string `json:"name"`
	Data string `json:"data"`
}
type windowsPayload struct {
	ParentTaskID   string         `json:"parent_task_id"`
	JobID          string         `json:"job_id"`
	Mode           string         `json:"mode"`
	SnapshotSHA256 string         `json:"snapshot_sha256"`
	Profile        windowsProfile `json:"profile"`
	Files          []windowsFile  `json:"files"`
}
type windowsReceipt struct {
	State  string                   `json:"state"`
	Result *model.EnvironmentResult `json:"result,omitempty"`
}

var phase0Files = []string{"CMakeLists.txt", "path_fixture.h", "path_fixture_test.cpp", "phase0.manifest", "README.md", "sqlite_path_probe.cpp"}

// Advertise configured host capabilities; remote connectivity/build checks remain preflight.
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
	}
	return map[string]model.ExecutionCapability{"windows_seekdb_phase0": cap}
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
	line := strings.SplitN(log[at+len(marker):], "\n", 2)[0]
	var result model.EnvironmentResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &result); err != nil {
		return nil, errors.New("invalid Windows result envelope")
	}
	if result.Status != "passed" && result.Status != "failed" && result.Status != "unavailable" {
		return nil, errors.New("invalid Windows verdict")
	}
	result.Log = log[:at]
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
	emit(agent.Event{Message: "Windows 执行器：快照已冻结，开始独立构建和 LongPathsEnabled=0/1 验证，完成后恢复原值。"})
	payload.Mode = "execute"
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
