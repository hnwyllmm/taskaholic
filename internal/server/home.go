package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"work-assistant/internal/model"
)

func (s *Server) registerHomeRoutes(mux *http.ServeMux) {
	for path, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/home":                                          s.handleHome,
		"GET /api/v1/home/chats":                                    s.handleHomeChats,
		"POST /api/v1/home/chats":                                   s.handleHomeCreate,
		"GET /api/v1/home/chats/{chat_id}":                          s.handleHomeChat,
		"POST /api/v1/home/chats/{chat_id}/messages":                s.handleHomeMessage,
		"POST /api/v1/home/chats/{chat_id}/stop":                    s.handleHomeStop,
		"POST /api/v1/home/chats/{chat_id}/proposals/{proposal_id}": s.handleHomeDecision,
	} {
		mux.Handle(path, s.apiAuth(handler))
	}
}
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListWork(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	type attention struct {
		Task     model.Task `json:"task"`
		Message  string     `json:"message"`
		ReviewID string     `json:"review_id,omitempty"`
	}
	items := []attention{}
	counts := map[string]int{}
	recent := []model.Task{}
	for _, t := range tasks {
		counts[t.State]++
		if len(recent) < 8 {
			recent = append(recent, t)
		}
		if t.State != model.TaskStateNew && t.State != model.TaskStateReview && t.State != model.TaskStateInput && t.State != model.TaskStateBlocked {
			continue
		}
		if len(items) >= 50 {
			continue
		}
		d, e := s.store.GetWorkDetail(r.Context(), t.ID)
		if e != nil {
			writeStoreError(w, e)
			return
		}
		a := attention{Task: t}
		if t.State == model.TaskStateNew {
			a.Message = "任务已保存，等待你指定执行成员或选择自动分派。"
			items = append(items, a)
			continue
		}
		for i := len(d.Messages) - 1; i >= 0; i-- {
			if d.Messages[i].Speaker != "user" {
				a.Message = d.Messages[i].Content
				break
			}
		}
		for _, review := range d.Reviews {
			if review.State == "PENDING" {
				a.ReviewID = review.ID
				break
			}
		}
		items = append(items, a)
	}
	reply(w, 200, map[string]any{"attention": items, "counts": counts, "recent": recent, "window_limit": 500}, nil)
}
func (s *Server) handleHomeChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.store.ListHomeChats(r.Context())
	for i := range chats {
		chats[i].Messages = nil
		chats[i].Proposals = nil
		chats[i].TaskVersions = nil
		chats[i].RoleVersions = nil
	}
	reply(w, 200, map[string]any{"chats": chats}, err)
}
func (s *Server) handleHomeCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	c, err := s.store.CreateHomeChat(r.Context(), req.Key)
	reply(w, 201, c, err)
}
func (s *Server) handleHomeChat(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetHomeChat(r.Context(), r.PathValue("chat_id"))
	reply(w, 200, c, err)
}
func (s *Server) handleHomeStop(w http.ResponseWriter, r *http.Request) {
	err := s.store.InterruptHomeChat(r.Context(), r.PathValue("chat_id"))
	reply(w, 202, map[string]any{"status": "interrupt_requested"}, err)
}
func (s *Server) handleHomeDecision(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Decision string `json:"decision"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	if req.Decision == "CONFIRMED" && !s.config.UpgradeEnabled {
		chat, err := s.store.GetHomeChat(r.Context(), r.PathValue("chat_id"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for _, proposal := range chat.Proposals {
			if proposal.ID == r.PathValue("proposal_id") && proposal.Kind == "upgrade_system" {
				writeStoreError(w, fmt.Errorf("%w: 当前启动方式没有升级守护进程，请通过启动脚本运行", model.ErrConflict))
				return
			}
		}
	}
	c, err := s.store.DecideHomeProposal(r.Context(), r.PathValue("chat_id"), r.PathValue("proposal_id"), req.Decision)
	reply(w, 200, c, err)
}
func (s *Server) handleHomeMessage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Message string `json:"message"`
		Version int64  `json:"expected_version"`
		Key     string `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err)
		return
	}
	c, err := s.store.GetHomeChat(r.Context(), r.PathValue("chat_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if input.Key != "" {
		if _, err = s.store.GetRunByKey(r.Context(), c.TaskID, input.Key); err == nil {
			reply(w, 200, c, nil)
			return
		} else if err != sql.ErrNoRows {
			writeStoreError(w, err)
			return
		}
	}
	runtimes, err := s.connectedRuntimes(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	req, err := s.systemExecutor(r.Context(), "home_chat", c.TaskID, input.Key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	upgradeStatus := "系统升级守护进程未启用"
	if s.config.UpgradeEnabled {
		upgradeStatus = "系统升级守护进程已启用；候选版本需要两次人工确认"
		upgrades, listErr := s.store.ListUpgrades(r.Context())
		if listErr != nil {
			writeStoreError(w, listErr)
			return
		}
		if len(upgrades) > 0 {
			upgradeStatus += fmt.Sprintf("；最近一次升级为 %q，状态 %s", upgrades[0].Title, upgrades[0].State)
		}
	}
	status := fmt.Sprintf("%d 台连接中的 Runtime，%d 个已配置员工；调度器已启用；当前普通工作执行模式为只读文本产物，不会自动修改仓库或合并 PR；%s。", len(runtimes), len(agents), upgradeStatus)
	c, err = s.store.StartHomeRun(r.Context(), c.ID, input.Version, input.Message, status, req, s.homeAssistant)
	reply(w, 202, c, err)
}
