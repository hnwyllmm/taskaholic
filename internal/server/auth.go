package server

import "net/http"

// Public UI configuration, not a login bypass. Never return credentials or
// infer this policy from client-supplied headers, query strings or network IPs.
func (s *Server) handleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"api_token_required": s.config.APIToken != ""})
}
