package dashboard

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/theshield2594/thicket/internal/memory"
	"github.com/theshield2594/thicket/internal/store"
)

// MemoryKeeper is the memory screen's engine side (the engine's
// ListMemories and friends): whose memories someone reaches, and that
// every change is recorded and can be undone.
type MemoryKeeper interface {
	ListMemories(ctx context.Context, u *store.User, sprig string) ([]memory.Entry, error)
	AddMemory(ctx context.Context, u *store.User, in memory.Input) (*memory.Entry, error)
	EditMemory(ctx context.Context, u *store.User, id int64, p memory.Patch) (*memory.Entry, error)
	DeleteMemory(ctx context.Context, u *store.User, id int64) error
	UndoMemory(ctx context.Context, u *store.User, id int64) (*memory.Entry, error)
}

// memBlock is one person's memory, or what one kid sprig remembers about
// its kids: facts and relationship memory apart.
type memBlock struct {
	Title, Note string
	Facts, Rel  []memGroup
	Count       int
	// The add form: which sprigs it may be for (Sprig fixes one), and
	// which kid (a kid sprig with several).
	Sprigs []option
	Sprig  string
	Kids   []option
}

type memGroup struct {
	Title    string
	Memories []memView
}

type memView struct {
	ID                             int64
	Kind, Content, Due, From, When string
	Person                         string
	Locked, Undoable               bool
}

type option struct{ Value, Label string }

// memories fills in the memory screen: the person's own, then each kid
// sprig they set up.
func (d *Dashboard) memories(ctx context.Context, u *store.User, p *pageData) error {
	for k := range memory.Kinds {
		if k != "followup" {
			p.MemKinds = append(p.MemKinds, k)
		}
	}
	sort.Slice(p.MemKinds, func(i, j int) bool { // relationship kinds last
		ri, rj := memory.Relationship[p.MemKinds[i]], memory.Relationship[p.MemKinds[j]]
		return ri != rj && rj || ri == rj && p.MemKinds[i] < p.MemKinds[j]
	})
	if d.Memories == nil {
		return nil
	}
	mems, err := d.Memories.ListMemories(ctx, u, "")
	if err != nil {
		return err
	}
	own := d.memBlock(mems, u)
	own.Sprigs = []option{{"", "every sprig (facts only)"}}
	for _, c := range d.Chars.For(u) {
		own.Sprigs = append(own.Sprigs, option{c.ID, c.DisplayName()})
	}
	p.MemBlocks = append(p.MemBlocks, own)
	p.MemCount = own.Count
	if u.IsKid() {
		return nil
	}
	for _, c := range d.Chars.All() {
		if !c.IsKid() || !c.IsParent(u.ID) {
			continue
		}
		mems, err := d.Memories.ListMemories(ctx, u, c.ID)
		if err != nil {
			return err
		}
		b := d.memBlock(mems, u)
		var names []string
		for _, id := range c.Kids {
			name := "Kid " + strconv.FormatInt(id, 10)
			if k, err := d.DB.UserByID(ctx, id); err == nil && k.Name != "" {
				name = k.Name
			}
			names = append(names, name)
			b.Kids = append(b.Kids, option{strconv.FormatInt(id, 10), name})
		}
		if len(b.Kids) < 2 {
			b.Kids = nil
		}
		b.Title = "What " + c.DisplayName() + " remembers about " + strings.Join(names, ", ")
		b.Note = "You set up " + c.DisplayName() + ", so you can see and change this. It stays between them and " + c.DisplayName() + "."
		b.Sprig = c.ID
		p.MemBlocks = append(p.MemBlocks, b)
	}
	return nil
}

func (d *Dashboard) memBlock(mems []memory.Entry, u *store.User) memBlock {
	var b memBlock
	userLoc := u.Location()
	facts, rel := map[string][]memView{}, map[string][]memView{}
	for _, m := range mems {
		v := memView{ID: m.ID, Kind: m.Kind, Content: m.Content, From: m.From, Locked: m.Locked, Undoable: m.Undoable,
			Person: m.PersonName, When: m.CreatedAt.In(userLoc).Format("2 Jan 2006")}
		if !m.UpdatedAt.Equal(m.CreatedAt) {
			v.When += ", changed " + m.UpdatedAt.In(userLoc).Format("2 Jan")
		}
		if !m.DueAt.IsZero() {
			v.Due = m.DueAt.In(userLoc).Format("Mon 2 Jan")
		}
		if m.Group == memory.GroupRelationship {
			key := "Shared with every sprig"
			if m.Sprig != "" {
				key = "With " + m.SprigName
			}
			rel[key] = append(rel[key], v)
		} else {
			key := "Shared with every sprig"
			if m.Sprig != "" {
				key = "Just with " + m.SprigName
			}
			facts[key] = append(facts[key], v)
		}
		b.Count++
	}
	b.Facts, b.Rel = groups(facts), groups(rel)
	return b
}

// groups orders memory groups: shared first, then by name.
func groups(m map[string][]memView) []memGroup {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	const shared = "Shared with every sprig"
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] == shared || (keys[j] != shared && keys[i] < keys[j])
	})
	var out []memGroup
	for _, k := range keys {
		out = append(out, memGroup{Title: k, Memories: m[k]})
	}
	return out
}

// memoryAct handles the memory screen's forms. It returns the flash code.
func (d *Dashboard) memoryAct(ctx context.Context, u *store.User, action string, id int64, r *http.Request) (string, error) {
	switch action {
	case "add":
		person, _ := strconv.ParseInt(r.PostFormValue("person"), 10, 64)
		_, err := d.Memories.AddMemory(ctx, u, memory.Input{Sprig: r.PostFormValue("sprig"), Kind: r.PostFormValue("mkind"),
			Content: r.PostFormValue("content"), Person: person})
		return "memory-added", err
	case "edit":
		content := r.PostFormValue("content")
		p := memory.Patch{Content: &content}
		if k := r.PostFormValue("mkind"); k != "" {
			p.Kind = &k
		}
		_, err := d.Memories.EditMemory(ctx, u, id, p)
		return "memory-saved", err
	case "lock", "unlock":
		locked := action == "lock"
		_, err := d.Memories.EditMemory(ctx, u, id, memory.Patch{Locked: &locked})
		return "memory-saved", err
	case "delete":
		return "memory-deleted", d.Memories.DeleteMemory(ctx, u, id)
	case "undo":
		_, err := d.Memories.UndoMemory(ctx, u, id)
		return "memory-undone", err
	}
	return "", errors.New("unknown action")
}
