package runtimehost

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
)

//go:embed vm_client.py
var vmClient string
var vmRequestName = regexp.MustCompile(`^[a-f0-9]{32}\.request$`)
var vmOperationID = regexp.MustCompile(`^[a-f0-9]{32}$`)

const vmOutputLimit = 8 * 1024 * 1024

type vmRequest struct {
	Operation   string `json:"operation"`
	OperationID string `json:"operation_id,omitempty"`
	Script      string `json:"script"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}
type vmResponse struct {
	ExitCode        int    `json:"exit_code"`
	Error           string `json:"error,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

type cappedWriter struct {
	destination io.Writer
	remaining   int64
	marker      string
	truncated   bool
}

func newCappedWriter(destination io.Writer, limit int64, stream string) *cappedWriter {
	return &cappedWriter{
		destination: destination,
		remaining:   limit,
		marker:      fmt.Sprintf("\n[work-assistant: Windows VM %s truncated after %d bytes; redirect large output to a file and inspect a focused excerpt.]\n", stream, limit),
	}
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	consumed := len(p)
	available := w.remaining
	if w.remaining > 0 {
		keep := int64(len(p))
		if keep > w.remaining {
			keep = w.remaining
		}
		n, err := w.destination.Write(p[:keep])
		if err != nil {
			return n, err
		}
		if int64(n) != keep {
			return n, io.ErrShortWrite
		}
		w.remaining -= keep
	}
	if int64(len(p)) > available && !w.truncated {
		w.truncated = true
		if _, err := io.WriteString(w.destination, w.marker); err != nil {
			return 0, err
		}
	}
	return consumed, nil
}

// The model gets a run-scoped mailbox, not host networking, commands or secrets.
func (d *Daemon) startVMBridge(ctx context.Context, spec model.RunSpec, workingDir, source string, emit func(agent.Event)) (string, func(), error) {
	noop := func() {}
	if spec.ExecutionGrant == nil || spec.ReadOnly || spec.Environment != nil {
		return "", noop, nil
	}
	raw, err := os.ReadFile(filepath.Join(d.config.WorkRoot, "windows-profiles.json"))
	if os.IsNotExist(err) {
		return "", noop, nil
	}
	if err != nil {
		return "", noop, err
	}
	var profiles map[string]windowsProfile
	if err = json.Unmarshal(raw, &profiles); err != nil {
		return "", noop, err
	}
	profile := profiles["windows_seekdb_phase0"]
	if !profile.Autonomous {
		return "", noop, nil
	}
	if !filepath.IsAbs(profile.WinRMCommand) {
		return "", noop, fmt.Errorf("Windows transport must be a configured absolute path")
	}
	// A connection preflight is cached by executionCapabilities.  Do not run an
	// extra probe for every turn (and do not make a just-started runtime depend
	// on that cache), but never hand an Agent a client after a known-bad health
	// result.  This is an explicit environment exception, not a mysterious
	// compiler failure later in the task.
	if health, known := d.cachedAutonomousWindowsCapability(profile); known && !health.Available {
		return "Windows VM 环境预检未通过：" + health.Reason + "。请提交 environment recovery request，说明需要恢复的 VM/WinRM/磁盘条件；不要猜测或替换仓库构建脚本。", noop, nil
	}
	dir := filepath.Join(workingDir, ".assistant-vm-"+spec.RunID)
	if err = os.Mkdir(dir, 0700); err != nil {
		return "", noop, err
	}
	mailbox, err := os.OpenRoot(dir)
	if err != nil {
		return "", noop, err
	}
	realSource, resolveErr := filepath.EvalSymlinks(source)
	if resolveErr != nil || realSource != source {
		mailbox.Close()
		return "", noop, fmt.Errorf("VM uploads require the actual task worktree, not a symlink")
	}
	workspace, err := os.OpenRoot(source)
	if err != nil {
		mailbox.Close()
		return "", noop, err
	}
	config, _ := json.Marshal(map[string]string{"workspace": source, "mailbox": dir})
	client := strings.Replace(vmClient, "__CONFIG_BASE64__", base64.StdEncoding.EncodeToString(config), 1)
	if err = mailbox.WriteFile("client.py", []byte(client), 0600); err != nil {
		mailbox.Close()
		workspace.Close()
		return "", noop, err
	}
	if err = mailbox.WriteFile("active", nil, 0600); err != nil {
		mailbox.Close()
		workspace.Close()
		return "", noop, err
	}
	// Normal agent turns may end while an already-submitted VM operation is
	// still running.  Keep those operations alive long enough to persist their
	// result for the next turn; only a real run cancellation should terminate
	// the transport process.
	operationCtx, cancelOperations := context.WithCancel(context.WithoutCancel(ctx))
	stopRequested := make(chan struct{})
	var stopOnce sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancelOperations()
		defer mailbox.Close()
		defer workspace.Close()
		defer mailbox.Remove("active")
		seen := map[string]bool{}
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		draining := false
		emptyDrainPass := false
		for {
			if draining {
				select {
				case <-ctx.Done():
					cancelOperations()
					return
				case <-ticker.C:
				}
			} else {
				select {
				case <-ctx.Done():
					cancelOperations()
					return
				case <-stopRequested:
					draining = true
				case <-ticker.C:
				}
			}
			entries, e := os.ReadDir(dir)
			if e != nil {
				return
			}
			processed := false
			for _, entry := range entries {
				name := entry.Name()
				if !vmRequestName.MatchString(name) || seen[name] {
					continue
				}
				processed = true
				seen[name] = true
				id := strings.TrimSuffix(name, ".request")
				reqData, e := mailbox.ReadFile(name)
				var request vmRequest
				if e == nil {
					e = json.Unmarshal(reqData, &request)
				}
				answer := vmResponse{ExitCode: 1}
				if e == nil {
					record := filepath.Join(d.config.WorkRoot, "vm-operations", spec.RunID, id)
					if e = os.MkdirAll(record, 0700); e == nil {
						e = durableWorkspaceJSON(filepath.Join(record, "request.json"), request)
						if e == nil {
							if request.Operation == "result" {
								answer = d.retrieveVMOperation(operationCtx, spec, request.OperationID, mailbox, record, id)
							} else {
								attributes := map[string]any{"operation_id": id, "operation": request.Operation}
								emit(agent.Event{Type: "vm.operation.queued", Message: "Windows VM 操作已排队：" + id + " (" + request.Operation + ")", Attributes: attributes})
								answer = d.executeVMOperationWithStart(operationCtx, profile.WinRMCommand, request, workspace, mailbox, record, id, func() {
									emit(agent.Event{Type: "vm.operation.started", Message: "Windows VM 操作开始：" + id + " (" + request.Operation + ")", Attributes: attributes})
								})
							}
							if persistErr := durableWorkspaceJSON(filepath.Join(record, "result.json"), answer); persistErr != nil {
								answer = vmResponse{ExitCode: 1, Error: "VM operation may have completed, but result persistence failed; inspect before retrying: " + persistErr.Error()}
							} else if request.Operation != "result" {
								emit(agent.Event{
									Type: "vm.operation.completed", Message: fmt.Sprintf("Windows VM 操作结束：%s，exit=%d。不是业务验收结论。", id, answer.ExitCode),
									Attributes: map[string]any{"operation_id": id, "operation": request.Operation, "exit_code": answer.ExitCode, "result_available": true},
								})
							}
						}
					}
				}
				if e != nil {
					answer.Error = e.Error()
				}
				data, _ := json.Marshal(answer)
				// Rooted operations cannot follow a model-created path outside its mailbox.
				if e = mailbox.WriteFile(id+".response.tmp", data, 0600); e != nil {
					return
				}
				if e = mailbox.Rename(id+".response.tmp", id+".response"); e != nil {
					return
				}
			}
			if draining {
				if processed {
					emptyDrainPass = false
				} else if emptyDrainPass {
					return
				} else {
					// Give a client that was already being scheduled when the
					// agent turn ended one final interval to publish its request.
					emptyDrainPass = true
				}
			}
		}
	}()
	stop := func() {
		stopOnce.Do(func() { close(stopRequested) })
		if ctx.Err() != nil {
			cancelOperations()
			<-done
		}
	}
	instructions := "\nWindows 虚拟机已获用户授权自主操作，不需要额外审批。通用工具：python3 " + filepath.Join(dir, "client.py") + " exec --script '<PowerShell>'，或 exec --file <本地脚本>；上传：python3 " + filepath.Join(dir, "client.py") + " upload <本地文件> <Windows目标路径>。若前一轮已经提交操作但当时尚未返回，使用本轮客户端的 result <operation-id> 读取持久化 stdout、stderr 和真实退出码；result 只读回执，不会再次执行 Windows 操作。你决定传什么代码、运行哪些仓库构建和测试命令、如何诊断修复。可以自行打包后上传，再在 VM 解包；上传没有 64 MiB 上限，不限六文件、固定产物或策略矩阵。控制台输出应保持精简；超过 8 MiB 会被截断并标记，完整构建日志或大型 JSON 请写入 VM 文件，再读取有针对性的摘要。PowerShell 的 Get-Content 结果嵌入 JSON 前应显式转换为字符串，避免序列化 Provider 扩展属性。不再提交 environment_request，填 null。不要使用旧专用验证流程。Windows 内部允许调整环境，操作后记录改变；这不授予 dev 宿主机权限，不读取宿主凭据、不修改控制系统、不扩大外部发布权限。上传源必须在本任务源码工作树内；系统只连接、传输、记录，不替你编排业务步骤。远端返回真实退出码；操作在同一台 Windows 虚拟机上串行执行，排队不代表失败。\n"
	return instructions, stop, nil
}

func (d *Daemon) executeVMOperation(ctx context.Context, transport string, r vmRequest, workspace, mailbox *os.Root, record, id string) vmResponse {
	return d.executeVMOperationWithStart(ctx, transport, r, workspace, mailbox, record, id, nil)
}

func (d *Daemon) executeVMOperationWithStart(ctx context.Context, transport string, r vmRequest, workspace, mailbox *os.Root, record, id string, started func()) vmResponse {
	fail := func(e error) vmResponse { return vmResponse{ExitCode: 1, Error: e.Error()} }
	if r.Operation != "exec" && r.Operation != "upload" {
		return fail(fmt.Errorf("unknown VM operation"))
	}
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
		return fail(ctx.Err())
	}
	if started != nil {
		started()
	}
	args := []string{}
	if r.Operation == "upload" {
		if r.Source == "" || filepath.IsAbs(r.Source) || r.Destination == "" {
			return fail(fmt.Errorf("upload requires a workspace-relative source and Windows destination"))
		}
		for _, part := range strings.FieldsFunc(r.Source, func(c rune) bool { return c == '/' || c == '\\' }) {
			if part == ".git" {
				return fail(fmt.Errorf("Git metadata is not an upload source"))
			}
		}
		input, e := workspace.Open(r.Source)
		if e != nil {
			return fail(e)
		}
		defer input.Close()
		info, e := input.Stat()
		if e != nil {
			return fail(e)
		}
		if !info.Mode().IsRegular() {
			return fail(fmt.Errorf("upload source must be a regular file"))
		}
		snapshot := filepath.Join(record, "upload.bin")
		output, e := os.OpenFile(snapshot, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return fail(e)
		}
		_, e = io.Copy(output, input)
		closeErr := output.Close()
		if e != nil {
			return fail(e)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
		args = []string{"--upload", snapshot, "--destination", r.Destination}
	} else if strings.TrimSpace(r.Script) == "" {
		return fail(fmt.Errorf("PowerShell script required"))
	}
	out, e := os.OpenFile(filepath.Join(record, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return fail(e)
	}
	defer out.Close()
	errout, e := os.OpenFile(filepath.Join(record, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return fail(e)
	}
	defer errout.Close()
	visible, e := mailbox.OpenFile(id+".stdout", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return fail(e)
	}
	defer visible.Close()
	visibleErr, e := mailbox.OpenFile(id+".stderr", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return fail(e)
	}
	defer visibleErr.Close()
	cmd := exec.CommandContext(ctx, transport, args...)
	// Give transports such as windows-run a chance to close their remote
	// command and shell before CommandContext falls back to a hard kill.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(r.Script)
	stdout := newCappedWriter(io.MultiWriter(out, visible), vmOutputLimit, "stdout")
	stderr := newCappedWriter(io.MultiWriter(errout, visibleErr), vmOutputLimit, "stderr")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	e = cmd.Run()
	if e != nil {
		exit := 1
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
			exit = cmd.ProcessState.ExitCode()
		}
		return vmResponse{ExitCode: exit, Error: e.Error(), StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated}
	}
	return vmResponse{ExitCode: 0, StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated}
}

// retrieveVMOperation copies an already-submitted operation's durable output
// into the current run mailbox. It deliberately bypasses windowsGate: status
// reads must remain available while a long Windows command owns the VM.
func (d *Daemon) retrieveVMOperation(ctx context.Context, current model.RunSpec, operationID string, mailbox *os.Root, record, requestID string) vmResponse {
	fail := func(code int, message string) vmResponse { return vmResponse{ExitCode: code, Error: message} }
	if !vmOperationID.MatchString(operationID) {
		return fail(1, "result requires a valid operation id")
	}
	operationsRoot := filepath.Join(d.config.WorkRoot, "vm-operations")
	runs, err := os.ReadDir(operationsRoot)
	if os.IsNotExist(err) {
		return fail(1, "VM operation is not available for this task session")
	}
	if err != nil {
		return fail(1, err.Error())
	}
	var previous string
	for _, entry := range runs {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(operationsRoot, entry.Name(), operationID)
		info, statErr := os.Stat(candidate)
		if statErr != nil || !info.IsDir() {
			continue
		}
		prior, found, loadErr := d.spool.LoadRunSpec(ctx, entry.Name())
		if loadErr != nil {
			return fail(1, loadErr.Error())
		}
		if found && prior.TaskID == current.TaskID && prior.SessionID == current.SessionID {
			previous = candidate
			break
		}
	}
	if previous == "" {
		return fail(1, "VM operation is not available for this task session")
	}
	encoded, err := os.ReadFile(filepath.Join(previous, "result.json"))
	if os.IsNotExist(err) {
		return fail(75, "VM operation "+operationID+" is still queued or running; do not submit it again, retry result later")
	}
	if err != nil {
		return fail(1, err.Error())
	}
	var result vmResponse
	if err = json.Unmarshal(encoded, &result); err != nil {
		return fail(1, "decode persisted VM result: "+err.Error())
	}
	for _, suffix := range []string{"stdout", "stderr"} {
		source := filepath.Join(previous, suffix+".log")
		input, openErr := os.Open(source)
		if os.IsNotExist(openErr) {
			continue
		}
		if openErr != nil {
			return fail(1, openErr.Error())
		}
		persisted, createErr := os.OpenFile(filepath.Join(record, suffix+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr != nil {
			input.Close()
			return fail(1, createErr.Error())
		}
		visible, createErr := mailbox.OpenFile(requestID+"."+suffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr != nil {
			input.Close()
			persisted.Close()
			return fail(1, createErr.Error())
		}
		_, copyErr := io.Copy(io.MultiWriter(persisted, visible), input)
		input.Close()
		persistErr := persisted.Close()
		visibleErr := visible.Close()
		if copyErr != nil {
			return fail(1, copyErr.Error())
		}
		if persistErr != nil {
			return fail(1, persistErr.Error())
		}
		if visibleErr != nil {
			return fail(1, visibleErr.Error())
		}
	}
	return result
}
