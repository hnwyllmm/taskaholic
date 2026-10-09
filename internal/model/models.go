package model

import (
	"fmt"
	"regexp"
)

// ModelOption is an adapter-specific model identifier, never a global model ID.
type ModelOption struct {
	ID                     string                  `json:"id"`
	Name                   string                  `json:"name"`
	ReasoningEfforts       []ReasoningEffortOption `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string                  `json:"default_reasoning_effort,omitempty"`
}

// ModelID is used only by adapters whose CLI encodes effort in model variants.
// It must refer to another entry discovered in the same adapter catalog.
type ReasoningEffortOption struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	ModelID     string `json:"model_id,omitempty"`
}

// Empty effort means inherit the native runtime configuration, NOT a known
// medium/low value. Configured confirms the CLI arguments, not model internals.
type ExecutionSettings struct {
	ReasoningEffort       string `json:"reasoning_effort,omitempty"`
	ReasoningEffortSource string `json:"reasoning_effort_source,omitempty"`
	ExecutionModelID      string `json:"execution_model_id,omitempty"`
	ExecutionConfigured   bool   `json:"execution_configured,omitempty"`
	// NetworkAccess is a Manager-selected runtime boundary for managed, read-only
	// Codex work such as review and verified source inspection. Agents cannot
	// request or set it through task input.
	NetworkAccess bool `json:"network_access,omitempty"`
}

var effortID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func ValidateReasoningEffort(value string) error {
	if value != "" && !effortID.MatchString(value) {
		return fmt.Errorf("%w: invalid reasoning_effort", ErrValidation)
	}
	return nil
}

// ResolveReasoningEffort never guesses an unknown/custom model's capabilities.
func ResolveReasoningEffort(models []ModelOption, modelID, effort string) (string, error) {
	if err := ValidateReasoningEffort(effort); err != nil {
		return "", err
	}
	if effort == "" {
		return modelID, nil
	}
	for _, m := range models {
		if m.ID != modelID {
			continue
		}
		for _, option := range m.ReasoningEfforts {
			if option.ID != effort {
				continue
			}
			if option.ModelID == "" {
				return modelID, nil
			}
			for _, target := range models {
				if target.ID == option.ModelID {
					return target.ID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%w: 当前模型未提供所选推理强度；请刷新模型列表或使用运行环境默认", ErrValidation)
}

// ModelCatalog is discovery metadata, not a promise of account quota or access.
// It does not change a member configuration or a pinned Session.
type ModelCatalog struct {
	Models      []ModelOption `json:"models"`
	Status      string        `json:"status"` // ready, unsupported, unavailable
	CheckedAtMS int64         `json:"checked_at_ms,omitempty"`
}
