package memory

import (
	"errors"
	"time"
)

// ErrNotFound is what someone gets for a memory that isn't theirs to
// see on the memory screen: the same as for one that doesn't exist.
var ErrNotFound = errors.New("no such memory of yours")

// Entry is one memory as the memory screen (web app, dashboard, API v2)
// shows it: which group it is in, whose it is, which sprig it is with, and
// who it came from and when.
type Entry struct {
	ID         int64     `json:"id"`
	Group      string    `json:"group"` // facts | relationship
	Kind       string    `json:"kind"`
	Content    string    `json:"content"`
	Scope      string    `json:"scope"` // shared (every sprig) | sprig (just one)
	Sprig      string    `json:"sprig,omitempty"`
	SprigName  string    `json:"sprig_name,omitempty"`
	Person     int64     `json:"person"` // whose memory it is
	PersonName string    `json:"person_name,omitempty"`
	Locked     bool      `json:"locked"`
	From       string    `json:"from"`             // who it came from, in words
	Origin     string    `json:"origin,omitempty"` // review | sprig:<id> | person:<id> | import
	DueAt      time.Time `json:"due_at,omitzero"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Undoable: a change made on the memory screen can still be undone.
	Undoable bool `json:"undoable"`
}

// Input is a memory a person adds on the memory screen.
type Input struct {
	Sprig   string `json:"sprig"` // "" = shared with every sprig (facts only)
	Kind    string `json:"kind"`
	Content string `json:"content"`
	// Person is whose memory it is: a kid, when a parent adds to a kid
	// sprig's memory (needed only if the sprig has several kids). Default:
	// the person adding it.
	Person int64 `json:"person,omitempty"`
	// Locked keeps the automatic review from changing it (default true).
	Locked *bool `json:"locked,omitempty"`
}

// Patch is an edit on the memory screen. Changing the content or kind
// locks the memory unless Locked says otherwise: the review won't undo
// what a person wrote.
type Patch struct {
	Content *string `json:"content,omitempty"`
	Kind    *string `json:"kind,omitempty"`
	Locked  *bool   `json:"locked,omitempty"`
}
