package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"work-assistant/internal/model"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(baseURL, token string) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid control URL %q", baseURL)
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), token: token,
		http: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *Client) CreateTask(ctx context.Context, title, goal, key string, requirements ...model.TaskRequirements) (model.Task, error) {
	var task model.Task
	var needs model.TaskRequirements
	if len(requirements) > 0 {
		needs = requirements[0]
	}
	err := c.json(ctx, http.MethodPost, "/api/v1/tasks", map[string]any{
		"title": title, "goal": goal, "idempotency_key": key, "requirements": needs,
	}, &task)
	return task, err
}

func (c *Client) ListTasks(ctx context.Context) ([]model.Task, error) {
	var response struct {
		Tasks []model.Task `json:"tasks"`
	}
	err := c.json(ctx, http.MethodGet, "/api/v1/tasks", nil, &response)
	return response.Tasks, err
}

func (c *Client) GetTask(ctx context.Context, taskID string) (model.TaskDetail, error) {
	var detail model.TaskDetail
	err := c.json(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, &detail)
	return detail, err
}

func (c *Client) CreateSubtask(ctx context.Context, parentTaskID, createdByRunID, title, goal, key string, requirements ...model.TaskRequirements) (model.SubtaskResult, error) {
	var result model.SubtaskResult
	var needs model.TaskRequirements
	if len(requirements) > 0 {
		needs = requirements[0]
	}
	err := c.json(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(parentTaskID)+"/subtasks", map[string]any{
		"title": title, "goal": goal, "created_by_run_id": createdByRunID, "idempotency_key": key, "requirements": needs,
	}, &result)
	return result, err
}

func (c *Client) CreateRun(ctx context.Context, taskID, sessionID, runtimeID, agentID, adapterID, modelID, workingDir, key string, command []string) (model.Run, error) {
	var run model.Run
	err := c.json(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/runs", map[string]any{
		"session_id": sessionID, "runtime_id": runtimeID, "agent_id": agentID,
		"adapter_id": adapterID, "model_id": modelID, "working_dir": workingDir,
		"idempotency_key": key, "command": command,
	}, &run)
	return run, err
}

func (c *Client) CreateDirective(ctx context.Context, runID, kind, message, key string) (model.Directive, error) {
	var directive model.Directive
	err := c.json(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(runID)+"/directives", map[string]any{
		"kind": kind, "message": message, "idempotency_key": key,
	}, &directive)
	return directive, err
}

func (c *Client) GetDirective(ctx context.Context, directiveID string) (model.Directive, error) {
	var directive model.Directive
	err := c.json(ctx, http.MethodGet, "/api/v1/directives/"+url.PathEscape(directiveID), nil, &directive)
	return directive, err
}

func (c *Client) ListSessions(ctx context.Context) ([]model.Session, error) {
	var response struct {
		Sessions []model.Session `json:"sessions"`
	}
	err := c.json(ctx, http.MethodGet, "/api/v1/sessions", nil, &response)
	return response.Sessions, err
}

func (c *Client) GetSession(ctx context.Context, sessionID string) (model.SessionDetail, error) {
	var detail model.SessionDetail
	err := c.json(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(sessionID), nil, &detail)
	return detail, err
}

func (c *Client) GetRun(ctx context.Context, runID string) (model.Run, error) {
	var run model.Run
	err := c.json(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID), nil, &run)
	return run, err
}

func (c *Client) InterruptRun(ctx context.Context, runID string) (map[string]any, error) {
	var result map[string]any
	err := c.json(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(runID)+"/interrupt", struct{}{}, &result)
	return result, err
}

func (c *Client) ListRuntimes(ctx context.Context) ([]model.Runtime, error) {
	var response struct {
		Runtimes []model.Runtime `json:"runtimes"`
	}
	err := c.json(ctx, http.MethodGet, "/api/v1/runtimes", nil, &response)
	return response.Runtimes, err
}

func (c *Client) WatchEvents(ctx context.Context, after int64, output io.Writer) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/events/stream?after=%d", c.baseURL, after), nil)
	if err != nil {
		return err
	}
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return responseError(response)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			if _, err := fmt.Fprintln(output, strings.TrimPrefix(line, "data: ")); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func (c *Client) Backup(ctx context.Context, destination string) error {
	if destination == "" {
		return errors.New("backup output path is required")
	}
	if _, err := os.Stat(destination); err == nil {
		return errors.New("backup output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/admin/backup", nil)
	if err != nil {
		return err
	}
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return responseError(response)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".work-assistant-backup-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, response.Body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

func (c *Client) json(ctx context.Context, method, path string, body, output any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	c.authorize(request)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return responseError(response)
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func (c *Client) authorize(request *http.Request) {
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func responseError(response *http.Response) error {
	limited, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(limited, &payload) == nil && payload.Error != "" {
		return fmt.Errorf("API %s: %s", response.Status, payload.Error)
	}
	return fmt.Errorf("API %s: %s", response.Status, strings.TrimSpace(string(limited)))
}
