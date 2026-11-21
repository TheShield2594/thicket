package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/theshield2594/thicket/internal/gallery"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

// The shared sprig gallery (THICKET_GALLERY_URL): a package is downloaded,
// checked against the index (size, SHA-256, its author's key,
// revocations) and against every gallery import rule, and the person sees
// the normal import preview before anything is installed. The web app's
// Gallery page (dashboard) and Juni's gallery_preview both come here.

// fetchGallery downloads and checks one package from the gallery.
func (e *Engine) fetchGallery(ctx context.Context, name string, version int) (*gallery.Fetched, error) {
	if e.Gallery == nil {
		return nil, errors.New("no gallery is set up (THICKET_GALLERY_URL)")
	}
	idx, err := e.Gallery.Index(ctx)
	if idx == nil {
		return nil, err
	}
	revoked, err := gallery.RevokedKeys(ctx, e.DB, e.Gallery)
	if err != nil {
		return nil, err
	}
	return e.Gallery.Fetch(ctx, idx, name, version, revoked)
}

// --- the web app's Gallery page ---

// GalleryIndex is the gallery's list for the Gallery page, and when it
// was fetched. An older copy comes with an error wrapping
// gallery.ErrStale.
func (e *Engine) GalleryIndex(ctx context.Context, u *store.User) (*sprigpkg.Index, time.Time, error) {
	if e.Gallery == nil {
		return nil, time.Time{}, errors.New("no gallery is set up (THICKET_GALLERY_URL)")
	}
	if u.IsKid() {
		return nil, time.Time{}, errors.New("the gallery is for grown-ups")
	}
	idx, err := e.Gallery.Index(ctx)
	_, at, _ := e.Gallery.Cached()
	return idx, at, err
}

// galleryToken is what the Gallery page's preview showed: the exact
// package and what importing it would do.
type galleryToken struct {
	SHA256 string       `json:"sha256"`
	Shown  galleryShown `json:"shown"`
}

// GalleryRevoked is every revoked author key id, for the Gallery page.
func (e *Engine) GalleryRevoked(ctx context.Context) []string {
	list, _ := gallery.RevokedKeys(ctx, e.DB, e.Gallery)
	return list
}

// GalleryPreview downloads and checks one package and returns the import
// preview, with a token naming exactly what was shown for GalleryInstall.
func (e *Engine) GalleryPreview(ctx context.Context, u *store.User, name string, version int) (preview, token string, err error) {
	if u.IsKid() {
		return "", "", errors.New("the gallery is for grown-ups")
	}
	f, err := e.fetchGallery(ctx, name, version)
	if err != nil {
		return "", "", err
	}
	g, trust, err := e.readGallery(ctx, u, f.Raw, "", "")
	if err != nil {
		var c *sprigpkg.Clash
		if errors.As(err, &c) {
			return "", "", fmt.Errorf("%w. Ask Juni to import it under another %s (suggest %q), or use thicket gallery install %s --%s", err, c.What, c.Suggest, f.Package.Name, c.What)
		}
		return "", "", err
	}
	text, err := g.Preview(trust)
	if err != nil {
		return "", "", err
	}
	s := sha256.Sum256(f.Raw)
	b, _ := json.Marshal(galleryToken{SHA256: hex.EncodeToString(s[:]), Shown: galleryShown{Sprig: g.Char.ID, Update: trust.Update, Skills: g.SkillIDs()}})
	return fmt.Sprintf("%s (version %d) from the gallery. This is everything it will get:\n\n%s", g.Char.DisplayName(), f.Version.Version, text), base64.StdEncoding.EncodeToString(b), nil
}

// GalleryInstall installs a package the person saw with GalleryPreview
// and approved: the same bytes (by SHA-256), checked again in full, doing
// what the preview said (a new import or an update, the same skills).
func (e *Engine) GalleryInstall(ctx context.Context, u *store.User, name string, version int, token string) (string, error) {
	if u.IsKid() {
		return "", errors.New("the gallery is for grown-ups")
	}
	var tok galleryToken
	b, err := base64.StdEncoding.DecodeString(token)
	if err != nil || json.Unmarshal(b, &tok) != nil || tok.SHA256 == "" {
		return "", errors.New("open the preview again: this form doesn't say what was shown")
	}
	f, err := e.fetchGallery(ctx, name, version)
	if err != nil {
		return "", err
	}
	if s := sha256.Sum256(f.Raw); hex.EncodeToString(s[:]) != tok.SHA256 {
		return "", errors.New("the gallery's package changed since you saw it, so nothing was installed; open the preview again")
	}
	shown := tok.Shown
	return e.importGallery(ctx, u, sprigImportArgs{File: f.File, Package: base64.StdEncoding.EncodeToString(f.Raw), Gallery: &shown}, f.Raw)
}

// GalleryOn reports whether a shared gallery is set up.
func (e *Engine) GalleryOn() bool { return e.Gallery != nil }
