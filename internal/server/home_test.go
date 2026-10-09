package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func TestHomePagesAndChatAPI(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{APIToken: "test"}, state, nil)
	for path, fragment := range map[string]string{
		"/":                       "系统进化",
		"/tasks":                  "工作列表",
		"/members":                "成员管理",
		"/members/":               "成员管理",
		"/roles":                  "成员管理",
		"/roles/":                 "成员管理",
		"/team":                   "团队资料",
		"/team/":                  "团队资料",
		"/projects":               "团队资料",
		"/projects/":              "团队资料",
		"/system":                 "数据保护",
		"/improvements":           "持续改进",
		"/assets/improvements.js": "gross_token_saving",
		"/assets/home.js":         "openDecision",
		"/assets/adapters.js":     "WAAdapters",
		"/assets/models.js":       "WAModels",
		"/assets/models.css":      "model-controls",
		"/assets/assignment.js":   "WAAssignment",
		"/assets/assignment.css":  "assignment-panel",
		"/assets/shared.js":       "主导航",
		"/assets/shell.css":       "home-grid",
		"/assets/pages.js":        "renderProjects",
	} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), fragment) || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("page %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/v1/home", "/api/v1/home/chats"} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatal("missing auth", path)
		}
	}
	call := func(method, path string, body any, want int, target any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if target != nil {
			if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
				t.Fatal(err)
			}
		}
	}
	var c model.HomeChat
	call("POST", "/api/v1/home/chats", map[string]any{"idempotency_key": "chat"}, 201, &c)
	call("POST", "/api/v1/home/chats/"+c.ID+"/messages", map[string]any{"message": "有哪些待办？", "expected_version": c.Version, "idempotency_key": "one"}, 409, nil)
	hello := model.RuntimeHello{RuntimeID: "runtime", Epoch: "epoch", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "read_only_runs": true, "structured_output": true}}}}
	if err = state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	s.hub.register(&runtimeConnection{runtimeID: hello.RuntimeID, epoch: hello.Epoch})
	request := map[string]any{"message": "有哪些待办？", "expected_version": c.Version, "idempotency_key": "one"}
	call("POST", "/api/v1/home/chats/"+c.ID+"/messages", request, 202, &c)
	if c.State != "GENERATING" {
		t.Fatal("not generating")
	}
	call("POST", "/api/v1/home/chats/"+c.ID+"/messages", request, 200, nil)
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: "runtime", Epoch: "epoch", RuntimeSeq: 1, RunID: c.LastRunID, Type: "run.completed", Output: `{"message":"目前没有工作。","proposal":null}`}); err != nil {
		t.Fatal(err)
	}
	call("GET", "/api/v1/home/chats/"+c.ID, nil, 200, &c)
	if c.State != "IDLE" || len(c.Messages) != 2 {
		t.Fatal("reply missing")
	}
	work, _ := state.ListWork(ctx)
	if len(work) != 0 {
		t.Fatal("status question became work")
	}
	call("POST", "/api/v1/home/chats/"+c.ID+"/proposals/fake", map[string]any{"decision": "APPROVED"}, 400, nil)
	call("POST", "/api/v1/home/chats/"+c.ID+"/proposals/fake", map[string]any{"decision": "CONFIRMED", "kind": "approve"}, 400, nil)
	call("POST", "/api/v1/home/chats/"+c.ID+"/proposals/fake", map[string]any{"decision": "CONFIRMED"}, 404, nil)
	call("GET", "/api/v1/home", nil, 200, nil)
}

func TestWorkQueueSummary(t *testing.T) {
	data, err := roleUI.ReadFile("ui/tasks.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, group := range []string{
		"['等待队列','queued']",
		"['推进中','active']",
		"['需要你','attention']",
		"['已完成','completed']",
	} {
		if !strings.Contains(body, group) {
			t.Fatalf("missing work summary group %s", group)
		}
	}
	if !strings.Contains(body, "WATaskHierarchy.bucket(t)===bucket") {
		t.Fatal("work summary must categorize each original task once")
	}
	data, err = roleUI.ReadFile("ui/tasks.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `<option value="QUEUED">等待队列</option>`) {
		t.Fatal("missing waiting queue filter")
	}
}

func TestUpgradeUIRequiresTwoConfirmations(t *testing.T) {
	for file, fragments := range map[string][]string{
		"home.html":   {"系统进化", "两次人工确认"},
		"home.js":     {"upgrade_system:'准备候选升级'", "确认安装并重启", "candidate_sha256", "等待当前运行结束", "查看具体代码改动", "候选升级待确认"},
		"system.html": {"隔离构建 · 两次确认 · 自动回滚", "升级守护进程 · 自动重启"},
	} {
		data, err := roleUI.ReadFile("ui/" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(data), fragment) {
				t.Fatalf("%s missing %q", file, fragment)
			}
		}
	}
}

func TestMemberManagementTerminology(t *testing.T) {
	for file, fragment := range map[string]string{
		"index.html":  "<h1>成员管理</h1>",
		"shared.js":   "['members','成员管理','/members']",
		"app.js":      "history.replaceState(null,'','/members')",
		"home.js":     "'/members#'",
		"tasks.js":    "请在成员管理页配置在线 Agent",
		"pages.js":    "'/members#'+a.role_id",
		"system.html": "打开成员管理",
		"shell.css":   "body[data-page=\"members\"]",
	} {
		t.Run(file, func(t *testing.T) {
			data, err := roleUI.ReadFile("ui/" + file)
			if err != nil {
				t.Fatal(err)
			}
			body := string(data)
			if !strings.Contains(body, fragment) {
				t.Fatalf("missing member management entry %q", fragment)
			}
			if strings.Contains(body, "角色配置") || strings.Contains(body, "角色页") {
				t.Fatal("stale user-facing page name")
			}
		})
	}
}

func TestTeamMaterialTerminology(t *testing.T) {
	for file, fragment := range map[string]string{
		"projects.html": "不会自动同步所有 Agent 的会话记忆",
		"shared.js":     "['team','团队资料','/team']",
		"tasks.html":    "不附加团队资料",
		"tasks.js":      "团队资料已保存",
		"pages.js":      "浏览资料",
		"system.html":   "团队资料",
	} {
		t.Run(file, func(t *testing.T) {
			data, err := roleUI.ReadFile("ui/" + file)
			if err != nil {
				t.Fatal(err)
			}
			body := string(data)
			if !strings.Contains(body, fragment) {
				t.Fatalf("missing team material copy %q", fragment)
			}
			if strings.Contains(body, "项目背景") || strings.Contains(body, "复制为新版") {
				t.Fatal("stale user-facing terminology")
			}
		})
	}
}
