package agent

import (
	"context"
	"fmt"
	"work-assistant/internal/model"
)

// Recheck at execution time: UI metadata may be stale, or the caller may bypass
// the HTTP API. Never silently drop a requested effort or retry with defaults.
func prepareReasoning(ctx context.Context, provider ModelProvider, spec model.RunSpec) (model.RunSpec, error) {
	if spec.ReasoningEffort != "" {
		models, err := provider.ListModels(ctx)
		if err != nil {
			return spec, fmt.Errorf("无法验证推理强度，模型列表暂不可用；未启动 Agent")
		}
		resolved, err := model.ResolveReasoningEffort(models, spec.ModelID, spec.ReasoningEffort)
		if err != nil {
			return spec, err
		}
		spec.ModelID = resolved
	}
	spec.ExecutionModelID = spec.ModelID
	spec.ExecutionConfigured = true
	return spec, nil
}
