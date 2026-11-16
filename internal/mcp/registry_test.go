package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowTransport answers tools/call after a delay and records Close.
type slowTransport struct {
	closed atomic.Bool
	delay  time.Duration
}

func (s *slowTransport) roundTrip(ctx context.Context, req rpcRequest) (*rpcResponse, error) {
	time.Sleep(s.delay)
	if s.closed.Load() {
		return nil, io.ErrClosedPipe
	}
	return &rpcResponse{Result: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)}, nil
}
func (s *slowTransport) notify(context.Context, rpcRequest) error { return nil }
func (s *slowTransport) close() error                             { s.closed.Store(true); return nil }

func TestReplacementWaitsForInFlightCalls(t *testing.T) {
	r := NewRegistry(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	old := &slowTransport{delay: 100 * time.Millisecond}
	r.conns["ha"] = &conn{client: &Client{t: old}}

	var wg sync.WaitGroup
	var res *CallResult
	var err error
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err = r.Call(context.Background(), "ha", "get_state", nil, true)
	}()
	time.Sleep(20 * time.Millisecond) // call is now in flight
	r.Disconnect("ha")                // e.g. a concurrent reconnect or removal
	if old.closed.Load() {
		t.Fatal("client closed while a call was still using it")
	}
	wg.Wait()
	if err != nil || res.Text != "ok" {
		t.Fatalf("in-flight call broken by replacement: %v %v", res, err)
	}
	if !old.closed.Load() {
		t.Fatal("retired client not closed after its last call finished")
	}
}
