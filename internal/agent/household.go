package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// SetRole changes someone's household role (admin, member or kid). Only
// admins may, nobody changes the owner's role, and it goes in the trail.
func (e *Engine) SetRole(ctx context.Context, by *store.User, userID int64, role string) error {
	if !by.IsAdmin() {
		return errors.New("only the owner or an admin can change roles")
	}
	if role != store.RoleAdmin && role != store.RoleMember && role != store.RoleKid {
		return errors.New("role must be admin, member or kid")
	}
	u, err := e.DB.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.Role == store.RoleOwner {
		return errors.New("the owner's role can't be changed")
	}
	if u.Role == role {
		return nil
	}
	if err := e.DB.SetUserRole(ctx, userID, role); err != nil {
		return err
	}
	name := u.Name
	if name == "" {
		name = fmt.Sprintf("user %d", u.ID)
	}
	e.addTrail(ctx, by.ID, "", store.TrailRole, fmt.Sprintf("%s: %s → %s", name, u.Role, role), "",
		&trailUndo{Type: "role", UserID: u.ID, Role: u.Role})
	e.logActivity(ctx, "info", "system", "", by.ID, fmt.Sprintf("%s's role changed to %s", name, role), "")
	return nil
}

// KidSprigSpec describes a new kid sprig.
type KidSprigSpec struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Role        string   `json:"role"` // one sentence: who it is
	Traits      []string `json:"traits"`
	VoiceStyle  []string `json:"voice_style"`
	Description string   `json:"description"`
	Kids        []int64  `json:"kids"`
	Parents     []int64  `json:"parents"`
	Color       string   `json:"color"`
}

// CreateKidSprig makes a kid sprig. Admins create them; the parents named
// then decide what it may use (KidAccess). It starts with no tools, no
// adult data and no initiative.
func (e *Engine) CreateKidSprig(ctx context.Context, by *store.User, s KidSprigSpec) (*character.Character, error) {
	if !by.IsAdmin() {
		return nil, errors.New("only the owner or an admin can create kid sprigs")
	}
	if _, exists := e.Chars.Get(s.ID); exists {
		return nil, fmt.Errorf("there's already a sprig called %q", s.ID)
	}
	for _, id := range s.Kids {
		u, err := e.DB.UserByID(ctx, id)
		if err != nil || !u.IsKid() {
			return nil, fmt.Errorf("user %d isn't one of the household's kids (make them a kid first)", id)
		}
	}
	for _, id := range s.Parents {
		u, err := e.DB.UserByID(ctx, id)
		if err != nil || u.IsKid() {
			return nil, fmt.Errorf("user %d isn't an adult in the household", id)
		}
	}
	if len(s.Traits) == 0 {
		s.Traits = []string{"kind", "patient", "playful"}
	}
	if len(s.VoiceStyle) == 0 {
		s.VoiceStyle = []string{"short, warm sentences a kid can follow", "never scary, never rude"}
	}
	c := &character.Character{
		ID: s.ID, Name: s.Name, Role: s.Role, Color: s.Color,
		Household: character.TierKid, Kids: slices.Clone(s.Kids), Parents: slices.Clone(s.Parents),
		Personality: character.Personality{Traits: s.Traits},
		Voice:       character.Voice{Style: s.VoiceStyle},
		CreatedBy:   "household",
		Body: strings.TrimSpace(s.Description + "\n\nYou are talking with a child. Keep things age-appropriate and kind. " +
			"Never ask for or encourage sharing personal details (address, school, phone, photos), never suggest meeting anyone, " +
			"and if they seem upset, unsafe or talk about hurting themselves, be gentle and encourage them to talk to a grown-up they trust."),
	}
	if probs := c.Validate(); len(probs) > 0 {
		return nil, errors.New(strings.Join(probs, "; "))
	}
	// A low cap that only its parents can raise, in place before the
	// sprig is live.
	capUSD := e.Settings.Float(ctx, "budget.kid_monthly_usd")
	undoCap, err := e.presetCap(ctx, c.ID, capUSD)
	if err != nil {
		return nil, err
	}
	existed := e.fileExists(c.ID)
	if _, err := e.Chars.Write(ctx, c); err != nil {
		// Write can fail after the file is in place (saving its hash):
		// take the file away first, so it never loads without its cap.
		e.unpublish(c.ID, existed)
		undoCap()
		return nil, err
	}
	e.addTrail(ctx, by.ID, c.ID, store.TrailGrant, fmt.Sprintf("kid sprig %s created for %v, parents %v", c.DisplayName(), c.Kids, c.Parents), "", nil)
	if capUSD > 0 {
		e.trailCap(ctx, by, c, "monthly", 0, capUSD)
	}
	e.logActivity(ctx, "info", "character", c.ID, by.ID, "kid sprig created", "")
	got, _ := e.Chars.Get(c.ID)
	return got, nil
}
