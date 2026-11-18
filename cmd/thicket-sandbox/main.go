// thicket-sandbox is the server in the sandbox container: the computer
// sprigs use. See internal/sandbox.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/theshield2594/thicket/internal/sandbox"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	s := &sandbox.Server{
		Workdir:   env("SANDBOX_WORKDIR", "/workspace"),
		TokenFile: env("SANDBOX_TOKEN_FILE", "/sandbox-auth/token"),
	}
	if err := os.MkdirAll(s.Workdir, 0o755); err != nil {
		log.Fatalf("workspace: %v", err)
	}
	srv := &http.Server{Addr: env("SANDBOX_ADDR", ":7070"), Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	log.Printf("thicket-sandbox listening on %s, workspace %s", srv.Addr, s.Workdir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
