// Package memory turns conversations into durable, scoped memories, and
// picks the relevant ones for each prompt.
//
// The model only proposes operations. Everything here validates them in
// code before anything is written: a weak model can't wreck memory.
package memory

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// Kinds of memory. "context" is short-lived small talk and expires.
var Kinds = map[string]bool{
	"fact": true, "preference": true, "person": true, "project": true,
	"inside_joke": true, "shared_history": true, "followup": true, "context": true,
}

const (
	MaxContent    = 300
	MaxOpsPerRun  = 20
	ContextTTL    = 14 * 24 * time.Hour
	dupeThreshold = 0.75
)

// Op is one change proposed by the review model.
type Op struct {
	Op      string `json:"op"`              // add | update | expire
	ID      int64  `json:"id,omitempty"`    // update, expire
	Scope   string `json:"scope,omitempty"` // add: "shared", "family" or a character id
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content,omitempty"`
	Due     string `json:"due,omitempty"` // followup: YYYY-MM-DD or YYYY-MM-DDTHH:MM
}

type PlanInput struct {
	UserID   int64
	Scopes   map[string]bool // character ids allowed besides "shared"
	Existing []store.Memory
	Now      time.Time
	Loc      *time.Location
	// Family allows scope "family": household facts from an adult's
	// conversation with family sprigs. Existing may then hold family
	// memories, which family adds are de-duplicated against.
	Family bool
}

type Plan struct {
	Changes []store.MemoryChange
	Skipped []string
}

// BuildPlan validates proposed ops and turns them into memory changes,
// merging near-duplicates and never touching user-locked memories.
func BuildPlan(in PlanInput, ops []Op) Plan {
	var p Plan
	if in.Loc == nil {
		in.Loc = time.UTC
	}
	if len(ops) > MaxOpsPerRun {
		p.Skipped = append(p.Skipped, fmt.Sprintf("%d ops proposed, only the first %d considered", len(ops), MaxOpsPerRun))
		ops = ops[:MaxOpsPerRun]
	}
	byID := map[int64]*store.Memory{}
	for i := range in.Existing {
		byID[in.Existing[i].ID] = &in.Existing[i]
	}
	touched := map[int64]bool{}
	// Adds accepted in this batch, for in-batch de-duplication.
	var pending []store.Memory

	for _, op := range ops {
		switch op.Op {
		case "add":
			content, err := cleanContent(op.Content)
			if err != nil {
				p.Skipped = append(p.Skipped, "add: "+err.Error())
				continue
			}
			scope := strings.TrimSpace(strings.ToLower(op.Scope))
			char := ""
			household := false
			switch {
			case scope == "family":
				if !in.Family {
					p.Skipped = append(p.Skipped, "add: family memory only comes from an adult's conversation with family sprigs")
					continue
				}
				household = true
			case scope != "" && scope != "shared":
				if !in.Scopes[scope] {
					p.Skipped = append(p.Skipped, fmt.Sprintf("add: scope %q is not a character in this conversation", op.Scope))
					continue
				}
				char = scope
			}
			kind := normKind(op.Kind)
			if !Kinds[kind] {
				p.Skipped = append(p.Skipped, fmt.Sprintf("add: unknown kind %q", op.Kind))
				continue
			}
			m := store.Memory{UserID: in.UserID, Character: char, Kind: kind, Content: content, Source: "auto", Household: household}
			if kind == "followup" && household {
				p.Skipped = append(p.Skipped, "add: follow-ups are private, not family memory")
				continue
			}
			if kind == "followup" {
				due, err := parseDue(op.Due, in.Now, in.Loc)
				if err != nil {
					p.Skipped = append(p.Skipped, "add followup: "+err.Error())
					continue
				}
				m.DueAt = due
			}
			if kind == "context" {
				m.ExpiresAt = in.Now.Add(ContextTTL)
			}
			if dup := findDuplicate(content, char, household, in.Existing); dup != nil {
				switch {
				case touched[dup.ID]:
				case dup.Locked:
					p.Skipped = append(p.Skipped, fmt.Sprintf("add: duplicate of user memory #%d", dup.ID))
				case sameText(dup.Content, content) && dup.Kind == kind && dup.DueAt.Equal(m.DueAt):
					p.Changes = append(p.Changes, store.MemoryChange{Action: "touch", Memory: store.Memory{ID: dup.ID}})
				default:
					ch := store.MemoryChange{Action: "update", Memory: store.Memory{ID: dup.ID, Content: content, Kind: kind, DueAt: m.DueAt}}
					expiryTransition(&ch, dup.Kind, kind, in.Now)
					p.Changes = append(p.Changes, ch)
				}
				touched[dup.ID] = true
				continue
			}
			if findDuplicate(content, char, household, pending) != nil {
				p.Skipped = append(p.Skipped, "add: duplicate within this review")
				continue
			}
			pending = append(pending, m)
			p.Changes = append(p.Changes, store.MemoryChange{Action: "add", Memory: m})

		case "update":
			old, ok := byID[op.ID]
			if !ok {
				p.Skipped = append(p.Skipped, fmt.Sprintf("update: memory #%d is not one of this user's memories", op.ID))
				continue
			}
			if old.Locked {
				p.Skipped = append(p.Skipped, fmt.Sprintf("update: memory #%d was set by the user and is locked", op.ID))
				continue
			}
			if touched[op.ID] {
				continue
			}
			content, err := cleanContent(op.Content)
			if err != nil {
				p.Skipped = append(p.Skipped, fmt.Sprintf("update #%d: %s", op.ID, err))
				continue
			}
			kind := ""
			if op.Kind != "" {
				if kind = normKind(op.Kind); !Kinds[kind] {
					kind = ""
				}
			}
			ch := store.Memory{ID: op.ID, Content: content, Kind: kind}
			effective := kind
			if effective == "" {
				effective = old.Kind
			}
			if op.Due != "" {
				due, err := parseDue(op.Due, in.Now, in.Loc)
				if err != nil {
					p.Skipped = append(p.Skipped, fmt.Sprintf("update #%d: %s", op.ID, err))
					continue
				}
				ch.DueAt = due
			}
			// A follow-up must always have a due date, as for adds.
			if effective == "followup" && ch.DueAt.IsZero() && old.DueAt.IsZero() {
				p.Skipped = append(p.Skipped, fmt.Sprintf("update #%d: a followup needs a due date", op.ID))
				continue
			}
			touched[op.ID] = true
			if sameText(content, old.Content) && effective == old.Kind && (ch.DueAt.IsZero() || ch.DueAt.Equal(old.DueAt)) {
				continue
			}
			change := store.MemoryChange{Action: "update", Memory: ch}
			expiryTransition(&change, old.Kind, effective, in.Now)
			p.Changes = append(p.Changes, change)

		case "expire":
			old, ok := byID[op.ID]
			if !ok {
				p.Skipped = append(p.Skipped, fmt.Sprintf("expire: memory #%d is not one of this user's memories", op.ID))
				continue
			}
			if old.Locked {
				p.Skipped = append(p.Skipped, fmt.Sprintf("expire: memory #%d was set by the user and is locked", op.ID))
				continue
			}
			if touched[op.ID] {
				continue
			}
			touched[op.ID] = true
			p.Changes = append(p.Changes, store.MemoryChange{Action: "expire", Memory: store.Memory{ID: op.ID}})

		default:
			p.Skipped = append(p.Skipped, fmt.Sprintf("unknown op %q", op.Op))
		}
	}
	return p
}

// expiryTransition sets the expiry when a memory moves into or out of the
// short-lived "context" kind, so it neither lingers nor vanishes wrongly.
func expiryTransition(ch *store.MemoryChange, from, to string, now time.Time) {
	switch {
	case from == to:
	case to == "context":
		ch.SetExpiry, ch.Memory.ExpiresAt = true, now.Add(ContextTTL)
	case from == "context":
		ch.SetExpiry, ch.Memory.ExpiresAt = true, time.Time{}
	}
}

func normKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	k = strings.ReplaceAll(k, "-", "_")
	k = strings.ReplaceAll(k, " ", "_")
	if k == "" {
		return "fact"
	}
	return k
}

func cleanContent(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	n := utf8.RuneCountInString(s)
	if n < 3 {
		return "", fmt.Errorf("content is empty")
	}
	if n > MaxContent {
		return "", fmt.Errorf("content too long (%d > %d chars)", n, MaxContent)
	}
	if secrets.LooksLikeSecret(s) {
		return "", fmt.Errorf("content looks like it contains a credential")
	}
	return s, nil
}

func parseDue(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("followup needs a due date")
	}
	var t time.Time
	var err error
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err = time.ParseInLocation(layout, s, loc); err == nil {
			if layout == "2006-01-02" {
				t = t.Add(10 * time.Hour) // date-only follow-ups land mid-morning
			}
			break
		}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("unreadable due date %q", s)
	}
	if t.Before(now.Add(-time.Hour)) {
		return time.Time{}, fmt.Errorf("due date %s is in the past", s)
	}
	if t.After(now.AddDate(1, 0, 0)) {
		return time.Time{}, fmt.Errorf("due date %s is more than a year out", s)
	}
	return t, nil
}

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "is": true, "are": true, "was": true, "were": true, "be": true, "it": true,
	"their": true, "they": true, "them": true, "user": true, "users": true, "has": true, "have": true,
	"with": true, "at": true, "by": true, "that": true, "this": true, "as": true, "from": true, "i": true,
	"me": true, "my": true, "you": true, "your": true, "we": true, "our": true, "about": true,
}

// Tokens splits text into lowercase content words.
func Tokens(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := f[:0]
	for _, w := range f {
		if len(w) > 1 && !stopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

// sameText reports whether two memories say exactly the same thing,
// ignoring case, spacing and trailing punctuation. It is deliberately
// stricter than Similarity: "blood type A" and "blood type B" differ.
func sameText(a, b string) bool {
	canon := func(s string) string {
		return strings.TrimRight(strings.ToLower(strings.Join(strings.Fields(s), " ")), ".!? ")
	}
	return canon(a) == canon(b)
}

func set(ws []string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// Similarity is the Jaccard overlap of content words, 0..1.
func Similarity(a, b string) float64 {
	sa, sb := set(Tokens(a)), set(Tokens(b))
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for w := range sa {
		if sb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

// findDuplicate finds a near-duplicate in the same layer: the same
// character scope, and family memory only against family memory.
func findDuplicate(content, char string, household bool, among []store.Memory) *store.Memory {
	var best *store.Memory
	bestScore := 0.0
	for i := range among {
		m := &among[i]
		if m.Character != char || m.Household != household {
			continue
		}
		if s := Similarity(content, m.Content); s >= dupeThreshold && s > bestScore {
			best, bestScore = m, s
		}
	}
	return best
}

var kindWeight = map[string]float64{
	"preference": 1.0, "fact": 1.0, "person": 0.9, "project": 0.9, "followup": 0.9,
	"inside_joke": 0.6, "shared_history": 0.6, "context": 0.5,
}

// Select picks the memories most relevant to query for a prompt, within a
// count and character budget. It returns them most relevant first.
func Select(mems []store.Memory, query string, now time.Time, maxItems, maxChars int) []store.Memory {
	q := set(Tokens(query))
	type scored struct {
		m store.Memory
		s float64
	}
	var all []scored
	for _, m := range mems {
		s := kindWeight[m.Kind]
		toks := set(Tokens(m.Content))
		overlap := 0
		for w := range toks {
			if q[w] {
				overlap++
			}
		}
		if len(toks) > 0 {
			s += 3 * float64(overlap) / math.Sqrt(float64(len(toks)))
		}
		if m.Locked {
			s += 0.5
		}
		if now.Sub(m.UpdatedAt) < 7*24*time.Hour {
			s += 0.3
		}
		if !m.LastUsedAt.IsZero() && now.Sub(m.LastUsedAt) < 24*time.Hour {
			s += 0.2
		}
		if m.Kind == "followup" && !m.DueAt.IsZero() && m.DueAt.Sub(now) < 3*24*time.Hour {
			s += 0.7
		}
		all = append(all, scored{m, s})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].s > all[j].s })
	var out []store.Memory
	chars := 0
	for _, x := range all {
		if len(out) >= maxItems {
			break
		}
		if chars+len(x.m.Content) > maxChars {
			continue
		}
		chars += len(x.m.Content)
		out = append(out, x.m)
	}
	return out
}
