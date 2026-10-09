// Package localworkspace contains the small amount of setup shared by the
// single-process personal deployment and the split control/runtime deployment.
package localworkspace

import (
	"context"
	"log/slog"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

// Bootstrap waits for the local runtime and idempotently creates the initial
// helper role and Agent. Existing profiles and user edits are never replaced.
func Bootstrap(ctx context.Context, state *store.Store, runtimeID, modelID, listen, adapterID string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			runtimes, err := state.ListRuntimes(ctx)
			if err != nil {
				return err
			}
			ready := false
			for _, runtime := range runtimes {
				if runtime.ID == runtimeID && runtime.State == "ONLINE" {
					ready = true
					break
				}
			}
			if !ready {
				continue
			}

			// Resolve stable setup identity before inspecting display names. A user
			// renaming the helper must not create a duplicate on restart.
			draft, err := state.CreateRoleDraft(ctx, "本机个人工作助手", "", "local-helper-role:"+runtimeID)
			if err != nil {
				return err
			}
			agents, err := state.ListAgents(ctx)
			if err != nil {
				return err
			}
			for _, profile := range agents {
				if draft.PublishedRoleID != "" && profile.RoleID == draft.PublishedRoleID && profile.RuntimeID == runtimeID {
					slog.Info("local workspace ready", "url", "http://"+listen+"/", "agent", profile.Name, "model", profile.ModelID)
					<-ctx.Done()
					return nil
				}
			}

			if draft.State != "PUBLISHED" {
				draft, err = state.UpdateRoleDraft(ctx, draft.ID, draft.Version, model.RoleSpec{
					Name:           "日常工作助手",
					Description:    "根据已有材料撰写文档、整理分析、提出代码建议，提交给人验收。",
					Capabilities:   []string{"document.write", "analysis", "code.suggest"},
					Instructions:   "理解任务及已有资料，必要时先提问；给出准确、清晰且可验证的交付结果。收到意见后修改原有产物。",
					OutputContract: "提供简短说明和完整的文档或文本文件；事实与推测分开；未执行的验证明确标注。",
					Boundaries:     []string{"不自行修改、发布或合并仓库", "不对外发送消息", "完成后交由人验收"},
				})
				if err != nil {
					return err
				}
			}
			role, err := state.PublishRoleDraft(ctx, draft.ID, draft.Version)
			if err != nil {
				return err
			}
			name := "本机工作助手"
			if adapterID == "cursor-agent" {
				name = "Cursor 工作助手"
			}
			if _, err = state.CreateAgent(ctx, model.AgentProfile{
				Name: name, RoleID: role.ID, RuntimeID: runtimeID, AdapterID: adapterID,
				ModelID: modelID, MaxConcurrent: 1,
			}); err != nil {
				return err
			}
			slog.Info("local workspace ready", "url", "http://"+listen+"/", "model", modelID)
			<-ctx.Done()
			return nil
		}
	}
}
