package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// The Settings screen: what owners and admins could already do by asking
// the host in chat (models and other settings, connectors), as forms.

// Admin is what the Settings screen asks of the engine. The values it
// returns are sent to the app as JSON.
type Admin interface {
	AdminSettings(ctx context.Context) any
	AdminSetSetting(ctx context.Context, by *store.User, name, value string) (any, error)
	AdminIntegrations(ctx context.Context) (any, error)
	AdminAddIntegration(ctx context.Context, by *store.User, name, url, token string) (any, error)
	AdminRemoveIntegration(ctx context.Context, by *store.User, name string) error
}

type userHandler = func(http.ResponseWriter, *http.Request, *store.User)

// adminOnly lets owners and admins through, when there is a Settings
// backend at all.
func (a *Adapter) adminOnly(next userHandler) userHandler {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if a.Admin == nil || !u.IsAdmin() {
			writeErr(w, http.StatusForbidden, "Only the owner or an admin can change settings.")
			return
		}
		next(w, r, u)
	}
}

func (a *Adapter) getSettings(w http.ResponseWriter, r *http.Request, _ *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"settings": a.Admin.AdminSettings(r.Context())})
}

func (a *Adapter) setSetting(w http.ResponseWriter, r *http.Request, u *store.User) {
	var b struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	s, err := a.Admin.AdminSetSetting(r.Context(), u, b.Name, b.Value)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *Adapter) getIntegrations(w http.ResponseWriter, r *http.Request, _ *store.User) {
	list, err := a.Admin.AdminIntegrations(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"integrations": list})
}

func (a *Adapter) addIntegration(w http.ResponseWriter, r *http.Request, u *store.User) {
	var b struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	in, err := a.Admin.AdminAddIntegration(r.Context(), u, b.Name, b.URL, b.Token)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

func (a *Adapter) removeIntegration(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := a.Admin.AdminRemoveIntegration(r.Context(), u, r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// saveProfile sets the signed-in person's name and timezone: what sprigs
// call them, and the clock their routines run on.
func (a *Adapter) saveProfile(w http.ResponseWriter, r *http.Request, u *store.User) {
	var b struct {
		Name     string `json:"name"`
		Timezone string `json:"timezone"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	b.Name, b.Timezone = strings.TrimSpace(b.Name), strings.TrimSpace(b.Timezone)
	if len(b.Name) > 60 {
		writeErr(w, http.StatusBadRequest, "That name is too long (60 characters at most).")
		return
	}
	if b.Timezone != "" {
		if _, err := time.LoadLocation(b.Timezone); err != nil {
			writeErr(w, http.StatusBadRequest, "Unknown timezone. Use a name like America/New_York.")
			return
		}
	}
	if err := a.DB.UpdateUserProfile(r.Context(), u.ID, b.Name, b.Timezone); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
