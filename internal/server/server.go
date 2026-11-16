// Package server exposes Thicket's HTTP surface: /health, character
// avatars, webhook triggers (/hooks), the local API (/api/v1), the
// read-only dashboard (/dashboard), and (mounted at "/") the web app.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// HookFunc starts the webhook job for token with the request body. It
// returns the HTTP status and a short message for the caller.
type HookFunc func(ctx context.Context, token string, body []byte, header http.Header) (int, string)

// Routes are the optional parts of the HTTP surface.
type Routes struct {
	Hooks       HookFunc     // POST /hooks/{token}
	MaxHookBody int          // bytes
	API         http.Handler // /api/v1/ (the web app owns the rest of /api/)
	Dashboard   http.Handler // /dashboard
	App         http.Handler // the web app, at "/"
}

func New(addr, version, charsDir string, health HealthFunc, rt Routes) *http.Server {
	mux := http.NewServeMux()
	if rt.API != nil {
		mux.Handle("/api/v1/", rt.API)
	}
	if rt.Dashboard != nil {
		mux.Handle("/dashboard", rt.Dashboard)
		mux.Handle("/dashboard/", rt.Dashboard)
	}
	if hooks, maxBody := rt.Hooks, rt.MaxHookBody; hooks != nil {
		mux.HandleFunc("POST /hooks/{token}", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(maxBody)))
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeJSON(w, http.StatusRequestEntityTooLarge, "body too large")
				return
			} else if err != nil {
				writeJSON(w, http.StatusBadRequest, "couldn't read the body")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			code, msg := hooks(ctx, r.PathValue("token"), body, r.Header)
			writeJSON(w, code, msg)
		})
	}
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
	if rt.App != nil {
		mux.Handle("/", rt.App)
	}
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

func writeJSON(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	key := "message"
	if code >= 400 {
		key = "error"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{key: msg})
}
