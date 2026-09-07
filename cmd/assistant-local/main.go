// assistant-local is the single-process personal deployment. The distributed
// assistantd / assistant-runtime binaries remain available independently.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/backup"
	"work-assistant/internal/model"
	"work-assistant/internal/runtimehost"
	"work-assistant/internal/server"
	"work-assistant/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "assistant-local:", err)
		os.Exit(1)
	}
}
func run() error {
	listen := flag.String("listen", "127.0.0.1:17343", "HTTP listen address; defaults to loopback")
	allowRemote := flag.Bool("allow-remote", false, "allow non-loopback HTTP listening; authentication is required unless --no-api-auth is explicit")
	noAPIAuth := flag.Bool("no-api-auth", false, "disable browser/control API authentication on a trusted network; runtime authentication is unchanged")
	dataDir := flag.String("data", "./data/local", "persistent data directory")
	runtimeID := flag.String("runtime-id", "local", "stable local runtime ID; retain it across restarts")
	modelID := flag.String("model", "", "model for the initial local helper agent; existing agents are never overwritten")
	binary := flag.String("codex-binary", "codex", "installed Codex CLI path")
	adapterID := flag.String("adapter", "codex-agent", "initial execution adapter: codex-agent or cursor-agent")
	extraAdapters := flag.String("extra-adapters", "", "additional execution adapters, comma-separated; does not change existing members or the initial helper")
	cursorBinary := flag.String("cursor-binary", "agent", "installed Cursor Agent CLI path")
	upgradeEnabled := flag.Bool("upgrade-enabled", false, "allow an external supervisor to apply confirmed upgrades")
	supervisorInstance := flag.String("supervisor-instance", "", "opaque supervisor instance used by health checks")
	upgradeValidationSandbox := flag.String("upgrade-validation-sandbox", "unavailable", "candidate validation sandbox maintained by the supervisor")
	flag.Parse()
	apiToken := os.Getenv("ASSISTANT_API_TOKEN")
	runtimeToken := os.Getenv("ASSISTANT_RUNTIME_TOKEN")
	if *noAPIAuth {
		apiToken = ""
		slog.Warn("control API authentication disabled; all reachable network clients can access data and operate tasks")
	}
	controlURL, err := localControlURL(*listen, *allowRemote, *noAPIAuth, apiToken, runtimeToken)
	if err != nil {
		return err
	}
	// Fail before touching presence or the database when a local instance is
	// already listening. This avoids a double-click taking the live runtime offline.
	probe, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("address %s is already in use; open the existing workspace instead: %w", *listen, err)
	}
	if err := probe.Close(); err != nil {
		return err
	}
	adapters, err := localAdapters(*adapterID, *extraAdapters, *binary, *cursorBinary)
	if err != nil {
		return err
	}
	state, err := store.OpenProtected(filepath.Join(*dataDir, "control.sqlite"))
	if err != nil {
		return err
	}
	defer state.Close()
	spool, err := runtimehost.OpenSpoolProtected(filepath.Join(*dataDir, "runtime.sqlite"))
	if err != nil {
		return err
	}
	defer spool.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	directory, err := backup.Directory(*dataDir)
	if err != nil {
		return err
	}
	backups, err := backup.New(backup.Config{Directory: directory, Sources: map[string]backup.Source{"control.sqlite": state, "runtime.sqlite": spool}})
	if err != nil {
		return err
	}
	if _, err := backups.Capture(ctx, "startup"); err != nil {
		return fmt.Errorf("required startup recovery point: %w", err)
	}
	control := server.New(server.Config{LocalRuntimeID: *runtimeID, Listen: *listen, APIToken: apiToken, RuntimeToken: runtimeToken, UpgradeEnabled: *upgradeEnabled, UpgradeValidationSandbox: *upgradeValidationSandbox, InstanceID: *supervisorInstance, Backups: backups}, state, nil)
	d, err := runtimehost.New(runtimehost.Config{RuntimeID: *runtimeID, ControlURL: controlURL, Token: runtimeToken, WorkRoot: filepath.Join(*dataDir, "workspaces")}, spool, nil, adapters...)
	if err != nil {
		return err
	}
	results := make(chan error, 3)
	go func() { results <- control.Run(ctx) }()
	go func() { results <- d.Run(ctx) }()
	go func() { results <- bootstrap(ctx, state, *runtimeID, *modelID, *listen, adapters[0].Name()) }()
	for i := 0; i < 3; i++ {
		e := <-results
		if e != nil {
			err = e
			cancel()
		} else if i < 2 && ctx.Err() != nil {
			cancel()
		}
	}
	return err
}

func localAdapters(primary, extra, codexBinary, cursorBinary string) ([]agent.Adapter, error) {
	names := []string{primary}
	if strings.TrimSpace(extra) != "" {
		names = append(names, strings.Split(extra, ",")...)
	}
	var adapters []agent.Adapter
	seen := make(map[string]bool)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if seen[name] {
			continue
		}
		a, err := localAdapter(name, codexBinary, cursorBinary)
		if err != nil {
			return nil, err
		}
		seen[name] = true
		adapters = append(adapters, a)
	}
	return adapters, nil
}

func localAdapter(adapterID, codexBinary, cursorBinary string) (agent.Adapter, error) {
	switch adapterID {
	case "codex-agent":
		return agent.NewCodexAdapter(codexBinary, "read-only")
	case "cursor-agent":
		return agent.NewCursorAdapter(cursorBinary)
	default:
		return nil, fmt.Errorf("unknown local adapter %q", adapterID)
	}
}

func bootstrap(ctx context.Context, s *store.Store, runtimeID, modelID, listen, adapterID string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			runtimes, err := s.ListRuntimes(ctx)
			if err != nil {
				return err
			}
			ready := false
			for _, r := range runtimes {
				if r.ID == runtimeID && r.State == "ONLINE" {
					ready = true
				}
			}
			if !ready {
				continue
			}
			// Resolve stable setup identity before inspecting display names. A user
			// renaming the helper must not cause a duplicate Agent on restart.
			draft, err := s.CreateRoleDraft(ctx, "本机个人工作助手", "", "local-helper-role:"+runtimeID)
			if err != nil {
				return err
			}
			agents, err := s.ListAgents(ctx)
			if err != nil {
				return err
			}
			for _, a := range agents {
				if draft.PublishedRoleID != "" && a.RoleID == draft.PublishedRoleID && a.RuntimeID == runtimeID {
					slog.Info("local workspace ready", "url", "http://"+listen+"/", "agent", a.Name, "model", a.ModelID)
					<-ctx.Done()
					return nil
				}
			}
			// Idempotent draft creation makes interrupted first-time setup resumable.
			if draft.State != "PUBLISHED" {
				draft, err = s.UpdateRoleDraft(ctx, draft.ID, draft.Version, model.RoleSpec{Name: "日常工作助手", Description: "根据已有材料撰写文档、整理分析、提出代码建议，提交给人验收。", Capabilities: []string{"document.write", "analysis", "code.suggest"}, Instructions: "理解任务及已有资料，必要时先提问；给出准确、清晰且可验证的交付结果。收到意见后修改原有产物。", OutputContract: "提供简短说明和完整的文档或文本文件；事实与推测分开；未执行的验证明确标注。", Boundaries: []string{"不自行修改、发布或合并仓库", "不对外发送消息", "完成后交由人验收"}})
				if err != nil {
					return err
				}
			}
			role, err := s.PublishRoleDraft(ctx, draft.ID, draft.Version)
			if err != nil {
				return err
			}
			name := "本机工作助手"
			if adapterID == "cursor-agent" {
				name = "Cursor 工作助手"
			}
			_, err = s.CreateAgent(ctx, model.AgentProfile{Name: name, RoleID: role.ID, RuntimeID: runtimeID, AdapterID: adapterID, ModelID: modelID, MaxConcurrent: 1})
			if err != nil {
				return err
			}
			slog.Info("local workspace ready", "url", "http://"+listen+"/", "model", modelID, "permissions", "read-only")
			<-ctx.Done()
			return nil
		}
	}
}
