package mcp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// A dropped connection is retried (with backoff, after reconnecting) only
// for read-only tools: for one that acts, nobody can tell whether it ran.
func TestRetriesOnlyReadOnlyCalls(t *testing.T) {
	var calls, drops atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		if !strings.Contains(body, `"id"`) {
			w.WriteHeader(202)
			return
		}
		_, idPart, _ := strings.Cut(body, `"id":`)
		var id int
		fmt.Sscanf(idPart, "%d", &id)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body, `"initialize"`):
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`, id)
		case strings.Contains(body, `"tools/list"`):
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, id)
		default:
			calls.Add(1)
			if drops.Load() > 0 {
				drops.Add(-1)
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close() // the connection drops mid-call
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"done"}]}}`, id)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	db, _ := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer db.Close()
	s := store.MCPServer{Name: "ha", Transport: "http", Target: srv.URL, Enabled: true}
	db.PutMCPServer(ctx, s)
	r := NewRegistry(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.RetryBackoff = time.Millisecond
	defer r.Close()
	if _, err := r.Connect(ctx, s); err != nil {
		t.Fatal(err)
	}

	drops.Store(2)
	res, err := r.Call(ctx, "ha", "get_state", nil, true)
	if err != nil || res.Text != "done" || calls.Load() != 3 {
		t.Fatalf("read-only call after two drops: %v %v (calls %d)", res, err, calls.Load())
	}

	calls.Store(0)
	drops.Store(1)
	if _, err := r.Call(ctx, "ha", "turn_on", nil, false); err == nil || !strings.Contains(err.Error(), "can't tell whether it took effect") {
		t.Fatalf("acting call: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("an acting call was retried (%d calls)", calls.Load())
	}

	drops.Store(5) // more than the retries
	if _, err := r.Call(ctx, "ha", "get_state", nil, true); err == nil {
		t.Fatal("gave up silently")
	}
}
