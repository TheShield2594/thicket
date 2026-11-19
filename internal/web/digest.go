package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/theshield2594/thicket/internal/store"
)

// The weekly digest, outside chat: each adult reads and changes only
// their own choice of it (always, skip_empty or off). The day and time
// are household settings, shown here but changed only in Settings.

// DigestPrefs is what the digest endpoints ask of the engine.
type DigestPrefs interface {
	DigestSchedule(ctx context.Context) (day, at string)
	SetDigestModeFor(ctx context.Context, u *store.User, mode string) error
	SetKidReportFor(ctx context.Context, u *store.User, mode string) error
}

func (a *Adapter) digestView(ctx context.Context, u *store.User) (map[string]any, error) {
	mode, err := a.DB.DigestMode(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	kids, err := a.DB.KidReportMode(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"mode": mode, "modes": []string{store.DigestAlways, store.DigestSkipEmpty, store.DigestOff},
		"day": "", "time": "", "timezone": u.Location().String(),
		"kids": kids, "kids_modes": []string{store.KidReportDaily, store.KidReportWeekly, store.KidReportOff}}
	if a.Digest != nil {
		out["day"], out["time"] = a.Digest.DigestSchedule(ctx)
	}
	return out, nil
}

// getDigest is the signed-in person's digest choice and the household's
// schedule.
func (a *Adapter) getDigest(w http.ResponseWriter, r *http.Request, u *store.User) {
	if u.IsKid() {
		writeErr(w, http.StatusForbidden, "The weekly digest is for adults.")
		return
	}
	out, err := a.digestView(r.Context(), u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// setDigest changes the signed-in person's own digest choice.
func (a *Adapter) setDigest(w http.ResponseWriter, r *http.Request, u *store.User) {
	if u.IsKid() {
		writeErr(w, http.StatusForbidden, "The weekly digest is for adults.")
		return
	}
	if a.Digest == nil {
		writeErr(w, http.StatusServiceUnavailable, "The weekly digest isn't available.")
		return
	}
	var b struct {
		Mode string `json:"mode"`
		// Kids is how a parent gets the report on their kids' sprigs.
		Kids string `json:"kids"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Mode != "" {
		mode := strings.ToLower(strings.TrimSpace(b.Mode))
		if !store.ValidDigestMode(mode) {
			writeErr(w, http.StatusBadRequest, "mode is always, skip_empty or off")
			return
		}
		if err := a.Digest.SetDigestModeFor(r.Context(), u, mode); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if b.Kids != "" {
		if err := a.Digest.SetKidReportFor(r.Context(), u, strings.ToLower(strings.TrimSpace(b.Kids))); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	a.getDigest(w, r, u)
}
