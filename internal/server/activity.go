package server

import (
	"fmt"
	"net/http"
	"strconv"
)

func nonnegativeCursor(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid event cursor")
	}
	return n, nil
}

func (s *Server) handleActivities(w http.ResponseWriter, r *http.Request) {
	before, err := nonnegativeCursor(r.URL.Query().Get("before"))
	if err != nil {
		writeError(w, 400, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := s.store.ListActivities(r.Context(), r.PathValue("task_id"), before, limit)
	reply(w, 200, page, err)
}
