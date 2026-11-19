package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

// fakeTodo is an MCP to-do list: add_item returns the new item's id as
// JSON, delete_item removes it.
type fakeTodo struct {
	mu    sync.Mutex
	items map[int]string
	next  int
}

func (f *fakeTodo) serve(t *testing.T) *httptest.Server {
	f.items = map[int]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`, *req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[`+
				`{"name":"add_item","description":"add an item to a list","inputSchema":{"type":"object"}},`+
				`{"name":"delete_item","description":"delete an item","inputSchema":{"type":"object"}}]}}`, *req.ID)
		case "tools/call":
			f.mu.Lock()
			var out string
			switch req.Params.Name {
			case "add_item":
				f.next++
				f.items[f.next] = fmt.Sprint(req.Params.Arguments["item"])
				out = fmt.Sprintf(`{"id": %d}`, f.next)
			case "delete_item":
				id, _ := req.Params.Arguments["id"].(float64)
				delete(f.items, int(id))
				out = "deleted"
			}
			f.mu.Unlock()
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}]}}`, *req.ID, out)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeTodo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

const tod = `---
id: tod
name: Tod
role: Keeps the shopping lists.
personality:
  traits: [tidy]
voice:
  style: [short]
tools: ["mcp:todo", schedule]
---
Tod keeps lists.
`

// A sprig at act level that adds something through an integration says so
// in the chat, in Thicket's words, with how to undo it; "undo" in reply
// removes it.
func TestActionTrailAndUndo(t *testing.T) {
	a, fc, _, ctx, _, _ := ready(t)
	todo := &fakeTodo{}
	srv := todo.serve(t)
	s := store.MCPServer{Name: "todo", Transport: "http", Target: srv.URL, Enabled: true,
		Undo: map[string]store.InverseCall{"add_item": {Tool: "delete_item", Args: map[string]string{"id": "{{result.id}}"},
			Said: "I added {{args.item}} to {{args.list}}", Label: "remove it"}}}
	if err := a.DB.PutMCPServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MCP.Connect(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "tod.md"), []byte(tod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	a.DB.SetToolLevel(ctx, "tod", store.AnyTool, store.LevelAct)

	a.Engine.HandleIncoming(ctx, dm("Tod, add milk to groceries"))
	out := fc.take()
	if todo.count() != 1 {
		t.Fatalf("item not added: %d", todo.count())
	}
	last := out[len(out)-1]
	if last.persona != "Tod" || last.text != "I added milk to groceries. Reply **undo** to remove it." {
		t.Fatalf("want the action said, with undo, after the reply: %+v", out)
	}
	line := fc.lastPost()
	trail, _ := a.DB.Trail(ctx, 0, 5)
	if len(trail) == 0 || trail[0].Kind != store.TrailAction || trail[0].Summary != "I added milk to groceries." || !trail[0].Undoable {
		t.Fatalf("trail entry: %+v", trail)
	}

	// "undo" in reply to that line removes it.
	in := dm("undo")
	in.ReplyTo = line
	a.Engine.HandleIncoming(ctx, in)
	out = fc.take()
	if todo.count() != 0 {
		t.Fatal("undo didn't remove the item")
	}
	if len(out) != 1 || !strings.HasPrefix(out[0].text, "Undone: I called todo's delete_item (id: 1)") {
		t.Fatalf("undo reply: %+v", out)
	}
	// Twice is harmless.
	a.Engine.HandleIncoming(ctx, in)
	if out := sentText(fc.take()); !strings.Contains(out, "already been undone") {
		t.Fatalf("second undo: %s", out)
	}

	// A bare "undo" reaches the newest action in this chat.
	a.Engine.HandleIncoming(ctx, dm("Tod, add eggs to groceries"))
	fc.take()
	a.Engine.HandleIncoming(ctx, dm("undo"))
	if out := sentText(fc.take()); todo.count() != 0 || !strings.Contains(out, "Undone") {
		t.Fatalf("bare undo: %d items, %s", todo.count(), out)
	}
	// Nothing left to undo: the sprig answers instead of Thicket.
	a.Engine.HandleIncoming(ctx, dm("undo"))
	if out := sentText(fc.take()); strings.Contains(out, "Undone") {
		t.Fatalf("undid something twice: %s", out)
	}

	// Scheduling a job is said too, and undo deletes the job.
	a.Engine.HandleIncoming(ctx, dm("Tod, schedule a reminder"))
	out = fc.take()
	if got := sentText(out); !strings.Contains(got, `I scheduled "Reminder". Reply **undo** to delete it.`) {
		t.Fatalf("schedule_job not said:\n%s", got)
	}
	jobs, _ := a.DB.CharacterJobs(ctx, "tod")
	if len(jobs) != 1 {
		t.Fatalf("jobs: %+v", jobs)
	}
	a.Engine.HandleIncoming(ctx, dm("undo"))
	fc.take()
	if jobs, _ := a.DB.CharacterJobs(ctx, "tod"); len(jobs) != 0 {
		t.Fatalf("job not deleted by undo: %+v", jobs)
	}
}

// A tool with no declared undo says so: before it runs at ask level, and
// after it ran.
func TestActionWithoutUndoSaysSo(t *testing.T) {
	a, fc, _, ctx, _, writes := ready(t)
	a.DB.SetToolLevel(ctx, "hal", "mcp_ha__turn_on", store.LevelAsk)
	a.Engine.HandleIncoming(ctx, dm("Hal, turn on the light"))
	if out := sentText(fc.take()); !strings.Contains(out, "This can't be undone once it's done.") {
		t.Fatalf("approval prompt should warn:\n%s", out)
	}
	a.Engine.HandleIncoming(ctx, dm("approve"))
	out := sentText(fc.take())
	if writes.Load() != 1 || !strings.Contains(out, "I used ha's turn_on (entity: light.kitchen). This can't be undone from here.") {
		t.Fatalf("after approval:\n%s", out)
	}
	trail, _ := a.DB.Trail(ctx, 0, 5)
	if trail[0].Kind != store.TrailAction || trail[0].Undoable {
		t.Fatalf("trail: %+v", trail[0])
	}
	// "undo" with nothing undoable goes to the sprig.
	a.Engine.HandleIncoming(ctx, dm("undo"))
	if out := sentText(fc.take()); strings.Contains(out, "Undone") {
		t.Fatalf("undid an action with no undo: %s", out)
	}
}

// A quiet sprig keeps routine work to the trail.
func TestQuietSprigKeepsRoutineWorkToTheTrail(t *testing.T) {
	a, fc, _, ctx, _, _ := ready(t)
	quiet := strings.Replace(tod, "tools:", "actions:\n  quiet: true\ntools:", 1)
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "tod.md"), []byte(quiet), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	a.Engine.HandleIncoming(ctx, dm("Tod, schedule a reminder"))
	if out := sentText(fc.take()); strings.Contains(out, "I scheduled") {
		t.Fatalf("a quiet sprig said routine work:\n%s", out)
	}
	trail, _ := a.DB.Trail(ctx, 0, 5)
	if len(trail) == 0 || !strings.Contains(trail[0].Summary, `I scheduled "Reminder"`) {
		t.Fatalf("routine work missing from the trail: %+v", trail)
	}
}
