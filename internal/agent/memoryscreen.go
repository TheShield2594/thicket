package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/memory"
	"github.com/theshield2594/thicket/internal/store"
)

// The memory screen (web app, dashboard, API v2): a person sees and
// changes what sprigs remember about them, facts apart from relationship
// memory. Every change goes through memory_events, and changes made here
// can be undone one by one. Deleting forgets for good.
//
// A person reaches their own private memories. A kid sprig's parents also
// reach what that sprig remembers about its kids (the kids' shared layer
// and the sprig's own space), so relationship memory with a kid stays in
// the kid sandbox: only the kid, the kid sprig and its parents see it.
// Family memory isn't on the screen: it changes through family sprigs.

// mayEditMemory reports whether u may see and change m on the screen.
func (e *Engine) mayEditMemory(u *store.User, m *store.Memory) bool {
	if m.Household {
		return false
	}
	if m.UserID == u.ID {
		return true
	}
	if u.IsKid() {
		return false
	}
	for _, c := range e.Chars.All() {
		if c.IsKid() && c.IsParent(u.ID) && slices.Contains(c.Kids, m.UserID) && (m.Character == "" || m.Character == c.ID) {
			return true
		}
	}
	return false
}

// parentedKidSprig is a kid sprig u set up, or nil.
func (e *Engine) parentedKidSprig(u *store.User, id string) *character.Character {
	c, ok := e.Chars.Get(id)
	if !ok || !c.IsKid() || u.IsKid() || !c.IsParent(u.ID) {
		return nil
	}
	return c
}

// ListMemories is what the memory screen shows. With no sprig: all of the
// person's private memories. With one of their sprigs: the shared layer
// and that sprig's own. With a kid sprig they set up: what it remembers
// about its kids.
func (e *Engine) ListMemories(ctx context.Context, u *store.User, sprig string) ([]memory.Entry, error) {
	var mems []store.Memory
	var err error
	switch {
	case sprig == "":
		mems, err = e.DB.AllMemories(ctx, u.ID)
	case e.parentedKidSprig(u, sprig) != nil:
		c := e.parentedKidSprig(u, sprig)
		mems, err = e.DB.KidSprigMemories(ctx, c.Kids, c.ID)
	default:
		c, ok := e.Chars.Get(sprig)
		if !ok || !c.UsableBy(u) {
			if ok && c.IsKid() {
				return nil, ErrNotParent
			}
			return nil, fmt.Errorf("no sprig %q of yours", sprig)
		}
		mems, err = e.DB.VisibleMemories(ctx, u.ID, c.ID)
	}
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(mems))
	for i, m := range mems {
		ids[i] = m.ID
	}
	undoable, err := e.DB.UndoableMemories(ctx, ids)
	if err != nil {
		return nil, err
	}
	names := e.memNames(ctx, u)
	out := make([]memory.Entry, 0, len(mems))
	for _, m := range mems {
		ent := names.entry(&m)
		ent.Undoable = undoable[m.ID]
		out = append(out, ent)
	}
	return out, nil
}

// AddMemory adds a memory from the screen, locked unless asked otherwise.
func (e *Engine) AddMemory(ctx context.Context, u *store.User, in memory.Input) (*memory.Entry, error) {
	person := u.ID
	sprig := strings.TrimSpace(strings.ToLower(in.Sprig))
	kidSprig := e.parentedKidSprig(u, sprig)
	switch {
	case kidSprig != nil:
		person = in.Person
		if person == 0 {
			if len(kidSprig.Kids) != 1 {
				return nil, fmt.Errorf("%s talks with several kids; say which one (person)", kidSprig.DisplayName())
			}
			person = kidSprig.Kids[0]
		}
		if !slices.Contains(kidSprig.Kids, person) {
			return nil, fmt.Errorf("%s doesn't talk with that person", kidSprig.DisplayName())
		}
	case in.Person != 0 && in.Person != u.ID:
		return nil, errors.New("you can only add to your own memories, or to a kid sprig's you set up")
	case sprig != "":
		if c, ok := e.Chars.Get(sprig); !ok || !c.UsableBy(u) {
			return nil, fmt.Errorf("no sprig %q of yours", in.Sprig)
		}
	}
	kind := memory.NormKind(in.Kind)
	if err := checkScreenKind(kind, sprig); err != nil {
		return nil, err
	}
	content, err := memory.CleanContent(in.Content)
	if err != nil {
		return nil, err
	}
	locked := in.Locked == nil || *in.Locked
	m := store.Memory{UserID: person, Character: sprig, Kind: kind, Content: content, Source: map[bool]string{true: "user", false: "auto"}[locked], Locked: locked,
		Origin: store.OriginPersonPrefix + strconv.FormatInt(u.ID, 10)}
	if kind == "context" {
		m.ExpiresAt = e.now().Add(memory.ContextTTL)
	}
	ids, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "add", Memory: m, By: u.ID}})
	if err != nil {
		return nil, err
	}
	return e.memEntry(ctx, u, ids[0])
}

// checkScreenKind: any kind but follow-ups (sprigs set those, with a
// date), and relationship memory only with a sprig.
func checkScreenKind(kind, sprig string) error {
	if !memory.Kinds[kind] || kind == "followup" {
		return fmt.Errorf("kind must be one of: %s", strings.Join(screenKinds(), ", "))
	}
	if memory.Relationship[kind] && sprig == "" {
		return fmt.Errorf("%s is relationship memory: it's with one sprig, so say which", kind)
	}
	return nil
}

func screenKinds() []string {
	var out []string
	for k := range memory.Kinds {
		if k != "followup" {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// EditMemory changes a memory's content, kind or lock from the screen.
func (e *Engine) EditMemory(ctx context.Context, u *store.User, id int64, p memory.Patch) (*memory.Entry, error) {
	m, err := e.screenMemory(ctx, u, id)
	if err != nil {
		return nil, err
	}
	if p.Content == nil && p.Kind == nil {
		if p.Locked == nil {
			return nil, errors.New("nothing to change: send content, kind or locked")
		}
		if *p.Locked != m.Locked {
			ch := store.MemoryChange{Action: "lock", Memory: store.Memory{ID: id, Locked: *p.Locked}, By: u.ID}
			if _, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{ch}); err != nil {
				return nil, err
			}
		}
		return e.memEntry(ctx, u, id)
	}
	content, kind := m.Content, m.Kind
	if p.Content != nil {
		if content, err = memory.CleanContent(*p.Content); err != nil {
			return nil, err
		}
	}
	if p.Kind != nil {
		kind = memory.NormKind(*p.Kind)
		if kind != m.Kind {
			if err := checkScreenKind(kind, m.Character); err != nil {
				return nil, err
			}
		}
	}
	locked := p.Locked == nil || *p.Locked
	ch := store.MemoryChange{Action: "update", Memory: store.Memory{ID: id, Content: content, Kind: kind, Locked: locked}, SetLock: true, By: u.ID}
	switch {
	case kind == m.Kind:
	case kind == "context":
		ch.SetExpiry, ch.Memory.ExpiresAt = true, e.now().Add(memory.ContextTTL)
	case m.Kind == "context":
		ch.SetExpiry = true
	}
	if _, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{ch}); err != nil {
		return nil, err
	}
	return e.memEntry(ctx, u, id)
}

// DeleteMemory forgets a memory for good, history and all.
func (e *Engine) DeleteMemory(ctx context.Context, u *store.User, id int64) error {
	if _, err := e.screenMemory(ctx, u, id); err != nil {
		return err
	}
	_, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "delete", Memory: store.Memory{ID: id}, By: u.ID}})
	return err
}

// UndoMemory undoes the newest change made to a memory on the screen. It
// returns nil when that change was adding it (the memory is gone).
func (e *Engine) UndoMemory(ctx context.Context, u *store.User, id int64) (*memory.Entry, error) {
	if _, err := e.screenMemory(ctx, u, id); err != nil {
		return nil, err
	}
	m, err := e.DB.UndoMemoryChange(ctx, id, u.ID)
	if err != nil || m == nil {
		return nil, err
	}
	return e.memEntry(ctx, u, id)
}

func (e *Engine) screenMemory(ctx context.Context, u *store.User, id int64) (*store.Memory, error) {
	m, err := e.DB.MemoryByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, memory.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if !m.ExpiresAt.IsZero() && !m.ExpiresAt.After(e.now()) {
		return nil, memory.ErrNotFound
	}
	if !e.mayEditMemory(u, m) {
		return nil, memory.ErrNotFound
	}
	return m, nil
}

func (e *Engine) memEntry(ctx context.Context, u *store.User, id int64) (*memory.Entry, error) {
	m, err := e.DB.MemoryByID(ctx, id)
	if err != nil {
		return nil, err
	}
	undoable, err := e.DB.UndoableMemories(ctx, []int64{id})
	if err != nil {
		return nil, err
	}
	ent := e.memNames(ctx, u).entry(m)
	ent.Undoable = undoable[id]
	return &ent, nil
}

// memNamer turns ids into the names the screen shows.
type memNamer struct {
	e      *Engine
	ctx    context.Context
	viewer *store.User
	people map[int64]string
}

func (e *Engine) memNames(ctx context.Context, viewer *store.User) *memNamer {
	return &memNamer{e: e, ctx: ctx, viewer: viewer, people: map[int64]string{}}
}

func (n *memNamer) sprig(id string) string {
	if c, ok := n.e.Chars.Get(id); ok {
		return c.DisplayName()
	}
	return id
}

func (n *memNamer) person(id int64) string {
	if id == n.viewer.ID {
		return "you"
	}
	if name, ok := n.people[id]; ok {
		return name
	}
	name := "someone"
	if u, err := n.e.DB.UserByID(n.ctx, id); err == nil && u.Name != "" {
		name = u.Name
	}
	n.people[id] = name
	return name
}

func (n *memNamer) entry(m *store.Memory) memory.Entry {
	ent := memory.Entry{ID: m.ID, Group: memory.GroupOf(m.Kind), Kind: m.Kind, Content: m.Content, Scope: "shared",
		Person: m.UserID, Locked: m.Locked, Origin: m.Origin, DueAt: m.DueAt, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
	if m.Character != "" {
		ent.Scope, ent.Sprig, ent.SprigName = "sprig", m.Character, n.sprig(m.Character)
	}
	if m.UserID != n.viewer.ID {
		ent.PersonName = n.person(m.UserID)
	}
	switch o := m.Origin; {
	case o == store.OriginReview:
		ent.From = "memory review"
	case o == store.OriginImport:
		ent.From = "imported"
	case strings.HasPrefix(o, store.OriginSprigPrefix):
		ent.From = n.sprig(strings.TrimPrefix(o, store.OriginSprigPrefix)) + ", in chat"
	case strings.HasPrefix(o, store.OriginPersonPrefix):
		id, _ := strconv.ParseInt(strings.TrimPrefix(o, store.OriginPersonPrefix), 10, 64)
		ent.From = n.person(id)
	case m.Source == "user":
		ent.From = "you, in chat"
	default:
		ent.From = "a chat"
	}
	return ent
}
