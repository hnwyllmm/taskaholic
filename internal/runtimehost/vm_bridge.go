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
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
)

//go:embed vm_client.py
var vmClient string
var vmRequestName = regexp.MustCompile(`^[a-f0-9]{32}\.request$`)

type vmRequest struct {
	Operation   string `json:"operation"`
	Script      string `json:"script"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}
type vmResponse struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
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
	child, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer mailbox.Close()
		defer workspace.Close()
		defer mailbox.Remove("active")
		seen := map[string]bool{}
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
			}
			entries, e := os.ReadDir(dir)
			if e != nil {
				return
			}
			for _, entry := range entries {
				name := entry.Name()
				if !vmRequestName.MatchString(name) || seen[name] {
					continue
				}
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
							emit(agent.Event{Message: "Windows VM 操作开始：" + id + " (" + request.Operation + ")"})
							answer = d.executeVMOperation(child, profile.WinRMCommand, request, workspace, mailbox, record, id)
							emit(agent.Event{Message: fmt.Sprintf("Windows VM 操作结束：%s，exit=%d。不是业务验收结论。", id, answer.ExitCode)})
							if persistErr := durableWorkspaceJSON(filepath.Join(record, "result.json"), answer); persistErr != nil {
								answer = vmResponse{ExitCode: 1, Error: "VM operation may have completed, but result persistence failed; inspect before retrying: " + persistErr.Error()}
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
		}
	}()
	stop := func() { cancel(); <-done }
	instructions := "\nWindows 虚拟机已获用户授权自主操作，不需要额外审批。通用工具：python3 " + filepath.Join(dir, "client.py") + " exec --script '<PowerShell>'，或 exec --file <本地脚本>；上传：python3 " + filepath.Join(dir, "client.py") + " upload <本地文件> <Windows目标路径>。你决定传什么代码、运行哪些仓库构建和测试命令、如何诊断修复。可以自行打包后上传，再在 VM 解包；没有 64 MiB 上限，不限六文件、固定产物或策略矩阵。不再提交 environment_request，填 null。不要使用旧专用验证流程。Windows 内部允许调整环境，操作后记录改变；这不授予 dev 宿主机权限，不读取宿主凭据、不修改控制系统、不扩大外部发布权限。上传源必须在本任务源码工作树内；系统只连接、传输、记录，不替你编排业务步骤。远端返回真实退出码，长命令可按执行工具的会话方式等待。\n"
	return instructions, stop, nil
}

func (d *Daemon) executeVMOperation(ctx context.Context, transport string, r vmRequest, workspace, mailbox *os.Root, record, id string) vmResponse {
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
	cmd.Stdin = strings.NewReader(r.Script)
	cmd.Stdout = io.MultiWriter(out, visible)
	cmd.Stderr = io.MultiWriter(errout, visibleErr)
	e = cmd.Run()
	if e != nil {
		exit := 1
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
			exit = cmd.ProcessState.ExitCode()
		}
		return vmResponse{ExitCode: exit, Error: e.Error()}
	}
	return vmResponse{ExitCode: 0}
}
