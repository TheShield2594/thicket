package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/theshield2594/thicket/internal/memory"
	"github.com/theshield2594/thicket/internal/store"
)

// MemoryKeeper is the memory screen's engine side (the engine's
// ListMemories and friends). Every rule is there: whose memories someone
// reaches, which kinds go where, and that changes are recorded and can be
// undone.
type MemoryKeeper interface {
	ListMemories(ctx context.Context, u *store.User, sprig string) ([]memory.Entry, error)
	AddMemory(ctx context.Context, u *store.User, in memory.Input) (*memory.Entry, error)
	EditMemory(ctx context.Context, u *store.User, id int64, p memory.Patch) (*memory.Entry, error)
	DeleteMemory(ctx context.Context, u *store.User, id int64) error
	UndoMemory(ctx context.Context, u *store.User, id int64) (*memory.Entry, error)
}

// memoryRoutes serves the memory screen under prefix (/api or /api/v2)
// with the given authentication.
func (a *Adapter) memoryRoutes(mux *http.ServeMux, prefix string, wrap func(authed) http.HandlerFunc) {
	mux.HandleFunc("GET "+prefix+"/memories", wrap(a.listMemories))
	mux.HandleFunc("POST "+prefix+"/memories", wrap(a.addMemory))
	mux.HandleFunc("PATCH "+prefix+"/memories/{id}", wrap(a.editMemory))
	mux.HandleFunc("DELETE "+prefix+"/memories/{id}", wrap(a.deleteMemory))
	mux.HandleFunc("PUT "+prefix+"/memories/{id}/lock", wrap(a.lockMemory))
	mux.HandleFunc("POST "+prefix+"/memories/{id}/undo", wrap(a.undoMemory))
}

func (a *Adapter) memoryErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, memory.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such memory of yours")
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

func (a *Adapter) memoriesReady(w http.ResponseWriter) bool {
	if a.Memories == nil {
		writeErr(w, http.StatusServiceUnavailable, "memories aren't available")
		return false
	}
	return true
}

// listMemories: ?sprig=<id> narrows to one sprig (the shared layer and
// its own), or, for a kid sprig's parent, shows what it remembers about
// its kids. Facts and relationship memory come apart.
func (a *Adapter) listMemories(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.memoriesReady(w) {
		return
	}
	mems, err := a.Memories.ListMemories(r.Context(), u, r.URL.Query().Get("sprig"))
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	facts, rel := []memory.Entry{}, []memory.Entry{}
	for _, m := range mems {
		if m.Group == memory.GroupRelationship {
			rel = append(rel, m)
		} else {
			facts = append(facts, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"facts": facts, "relationship": rel})
}

func (a *Adapter) addMemory(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.memoriesReady(w) {
		return
	}
	var in memory.Input
	if !decodeBody(w, r, &in) {
		return
	}
	m, err := a.Memories.AddMemory(r.Context(), u, in)
	if err != nil {
		a.memoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"memory": m})
}

func memoryID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (a *Adapter) editMemory(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.memoriesReady(w) {
		return
	}
	var p memory.Patch
	if !decodeBody(w, r, &p) {
		return
	}
	m, err := a.Memories.EditMemory(r.Context(), u, memoryID(r), p)
	if err != nil {
		a.memoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory": m})
}

func (a *Adapter) lockMemory(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.memoriesReady(w) {
		return
	}
	var b struct {
		Locked *bool `json:"locked"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Locked == nil {
		writeErr(w, http.StatusBadRequest, `send {"locked": true} or {"locked": false}`)
		return
	}
	m, err := a.Memories.EditMemory(r.Context(), u, memoryID(r), memory.Patch{Locked: b.Locked})
	if err != nil {
		a.memoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory": m})
}

func (a *Adapter) deleteMemory(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.memoriesReady(w) {
		return
	}
	if err := a.Memories.DeleteMemory(r.Context(), u, memoryID(r)); err != nil {
		a.memoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Adapter) undoMemory(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.memoriesReady(w) {
		return
	}
	m, err := a.Memories.UndoMemory(r.Context(), u, memoryID(r))
	if err != nil {
		a.memoryErr(w, err)
		return
	}
	// memory is null when the undone change was adding it.
	writeJSON(w, http.StatusOK, map[string]any{"memory": m})
}
