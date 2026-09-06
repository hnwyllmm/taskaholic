package apiclient

import (
	"context"
	"net/http"
	"net/url"

	"work-assistant/internal/model"
)

func (c *Client) CreateRoleDraft(ctx context.Context, description, sourceRoleID, key string) (model.RoleDraft, error) {
	var draft model.RoleDraft
	err := c.json(ctx, http.MethodPost, "/api/v1/role-drafts", map[string]any{"description": description, "source_role_id": sourceRoleID, "idempotency_key": key}, &draft)
	return draft, err
}
func (c *Client) GetRoleDraft(ctx context.Context, id string) (model.RoleDraft, error) {
	var draft model.RoleDraft
	err := c.json(ctx, http.MethodGet, "/api/v1/role-drafts/"+url.PathEscape(id), nil, &draft)
	return draft, err
}
func (c *Client) ListRoleDrafts(ctx context.Context) ([]model.RoleDraft, error) {
	var result struct {
		Drafts []model.RoleDraft `json:"drafts"`
	}
	err := c.json(ctx, http.MethodGet, "/api/v1/role-drafts", nil, &result)
	return result.Drafts, err
}
func (c *Client) UpdateRoleDraft(ctx context.Context, id string, expected int64, spec model.RoleSpec) (model.RoleDraft, error) {
	var draft model.RoleDraft
	err := c.json(ctx, http.MethodPut, "/api/v1/role-drafts/"+url.PathEscape(id), map[string]any{"expected_version": expected, "spec": spec}, &draft)
	return draft, err
}

type RoleGeneration struct {
	Draft model.RoleDraft `json:"draft"`
	Run   model.Run       `json:"run"`
}

func (c *Client) ChatRoleDraft(ctx context.Context, id string, expected int64, message, runtimeID, adapterID, modelID, key string) (RoleGeneration, error) {
	var result RoleGeneration
	err := c.json(ctx, http.MethodPost, "/api/v1/role-drafts/"+url.PathEscape(id)+"/messages", map[string]any{"expected_version": expected, "message": message, "runtime_id": runtimeID, "adapter_id": adapterID, "model_id": modelID, "idempotency_key": key}, &result)
	return result, err
}
func (c *Client) PublishRoleDraft(ctx context.Context, id string, expected int64) (model.Role, error) {
	var role model.Role
	err := c.json(ctx, http.MethodPost, "/api/v1/role-drafts/"+url.PathEscape(id)+"/publish", map[string]any{"expected_version": expected}, &role)
	return role, err
}
func (c *Client) ListRoles(ctx context.Context) ([]model.Role, error) {
	var result struct {
		Roles []model.Role `json:"roles"`
	}
	err := c.json(ctx, http.MethodGet, "/api/v1/roles", nil, &result)
	return result.Roles, err
}
func (c *Client) GetRole(ctx context.Context, id string) (model.Role, error) {
	var role model.Role
	err := c.json(ctx, http.MethodGet, "/api/v1/roles/"+url.PathEscape(id), nil, &role)
	return role, err
}
func (c *Client) CreateAgent(ctx context.Context, agent model.AgentProfile) (model.AgentProfile, error) {
	var result model.AgentProfile
	err := c.json(ctx, http.MethodPost, "/api/v1/agents", map[string]any{"name": agent.Name, "role_id": agent.RoleID, "runtime_id": agent.RuntimeID, "adapter_id": agent.AdapterID, "model_id": agent.ModelID, "max_concurrent": agent.MaxConcurrent}, &result)
	return result, err
}
func (c *Client) ListAgents(ctx context.Context) ([]model.AgentProfile, error) {
	var result struct {
		Agents []model.AgentProfile `json:"agents"`
	}
	err := c.json(ctx, http.MethodGet, "/api/v1/agents", nil, &result)
	return result.Agents, err
}
