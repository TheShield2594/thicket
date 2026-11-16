// Package server exposes Thicket's HTTP surface: /health, character
// avatars, and (mounted at "/") the web app.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type Check struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// HealthFunc returns named checks; any failing check makes /health 503.
type HealthFunc func(ctx context.Context) map[string]Check

func New(addr, version, charsDir string, health HealthFunc, app http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		checks := health(ctx)
		ok := true
		for _, c := range checks {
			ok = ok && c.OK
		}
		status := "ok"
		code := http.StatusOK
		if !ok {
			status, code = "degraded", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "version": version, "checks": checks})
	})
	mux.HandleFunc("GET /avatars/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.PathValue("file"))
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".gif" && ext != ".webp" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeFile(w, r, filepath.Join(charsDir, name))
	})
	if app != nil {
		mux.Handle("/", app)
	}
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}
