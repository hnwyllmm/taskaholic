package server

import (
	"embed"
	"net/http"
)

//go:embed ui/*
var roleUI embed.FS

func (s *Server) handlePageUI(w http.ResponseWriter, r *http.Request) {
	var path, contentType string
	switch r.URL.Path {
	case "/":
		path, contentType = "home.html", "text/html; charset=utf-8"
	case "/system", "/system/":
		path, contentType = "system.html", "text/html; charset=utf-8"
	case "/team", "/team/", "/projects", "/projects/":
		path, contentType = "projects.html", "text/html; charset=utf-8"
	case "/sources", "/sources/":
		path, contentType = "sources.html", "text/html; charset=utf-8"
	case "/assets/sources.js":
		path, contentType = "sources.js", "text/javascript; charset=utf-8"
	case "/assets/sources.css":
		path, contentType = "sources.css", "text/css; charset=utf-8"
	case "/assets/shell.css":
		path, contentType = "shell.css", "text/css; charset=utf-8"
	case "/assets/shared.js":
		path, contentType = "shared.js", "text/javascript; charset=utf-8"
	case "/assets/system-agents.js":
		path, contentType = "system-agents.js", "text/javascript; charset=utf-8"
	case "/assets/adapters.js":
		path, contentType = "adapters.js", "text/javascript; charset=utf-8"
	case "/assets/models.js":
		path, contentType = "models.js", "text/javascript; charset=utf-8"
	case "/assets/models.css":
		path, contentType = "models.css", "text/css; charset=utf-8"
	case "/assets/assignment.js":
		path, contentType = "assignment.js", "text/javascript; charset=utf-8"
	case "/assets/task-hierarchy.js":
		path, contentType = "task-hierarchy.js", "text/javascript; charset=utf-8"
	case "/assets/test-pipelines.js":
		path, contentType = "test-pipelines.js", "text/javascript; charset=utf-8"
	case "/assets/task-references.js":
		path, contentType = "task-references.js", "text/javascript; charset=utf-8"
	case "/assets/assignment.css":
		path, contentType = "assignment.css", "text/css; charset=utf-8"
	case "/assets/home.js":
		path, contentType = "home.js", "text/javascript; charset=utf-8"
	case "/assets/review-chat.js":
		path, contentType = "review-chat.js", "text/javascript; charset=utf-8"
	case "/assets/activity.js":
		path, contentType = "activity.js", "text/javascript; charset=utf-8"
	case "/assets/pages.js":
		path, contentType = "pages.js", "text/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := roleUI.ReadFile("ui/" + path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	_, _ = w.Write(data)
}

func (s *Server) handleWorkUI(w http.ResponseWriter, r *http.Request) {
	var path, contentType string
	switch r.URL.Path {
	case "/tasks", "/tasks/":
		path, contentType = "ui/tasks.html", "text/html; charset=utf-8"
	case "/tasks/tasks.js":
		path, contentType = "ui/tasks.js", "text/javascript; charset=utf-8"
	case "/tasks/tasks.css":
		path, contentType = "ui/tasks.css", "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := roleUI.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	_, _ = w.Write(data)
}

func (s *Server) handleRoleUI(w http.ResponseWriter, r *http.Request) {
	path := "ui/index.html"
	contentType := "text/html; charset=utf-8"
	switch r.URL.Path {
	case "/members", "/members/", "/roles", "/roles/":
	case "/roles/app.js":
		path, contentType = "ui/app.js", "text/javascript; charset=utf-8"
	case "/roles/style.css":
		path, contentType = "ui/style.css", "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := roleUI.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	_, _ = w.Write(data)
}
