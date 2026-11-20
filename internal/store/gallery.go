package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GalleryKey is a gallery author's key, trusted on first use.
type GalleryKey struct {
	KeyID      string
	PublicKey  string // base64
	FirstSeen  time.Time
	FirstSprig string
	TrustedBy  int64
}

// GalleryKey returns a known author key, or ErrNotFound.
func (s *Store) GalleryKey(ctx context.Context, keyID string) (*GalleryKey, error) {
	var k GalleryKey
	var at int64
	var by sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT key_id, public_key, first_seen, first_sprig, trusted_by FROM gallery_keys WHERE key_id = ?", keyID).
		Scan(&k.KeyID, &k.PublicKey, &at, &k.FirstSprig, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.FirstSeen, k.TrustedBy = time.Unix(at, 0), by.Int64
	return &k, nil
}

// TrustGalleryKey stores an author key the first time a package signed
// with it is imported. A key already stored keeps its first record; a
// different public key under the same id is refused.
func (s *Store) TrustGalleryKey(ctx context.Context, k GalleryKey) error {
	cur, err := s.GalleryKey(ctx, k.KeyID)
	switch {
	case err == nil && cur.PublicKey == k.PublicKey:
		return nil
	case err == nil:
		return errors.New("a different key is already stored under this key id")
	case !errors.Is(err, ErrNotFound):
		return err
	}
	var by any
	if k.TrustedBy != 0 {
		by = k.TrustedBy
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO gallery_keys (key_id, public_key, first_seen, first_sprig, trusted_by) VALUES (?, ?, ?, ?, ?)",
		k.KeyID, k.PublicKey, s.unix(), k.FirstSprig, by)
	return err
}

// GallerySprig records which author key a sprig from the gallery came
// with.
type GallerySprig struct {
	SprigID    string
	PackageID  string // the id in the package's manifest
	KeyID      string
	ImportedAt time.Time
	UpdatedAt  time.Time
}

// GallerySprig returns a sprig's gallery record, or ErrNotFound.
func (s *Store) GallerySprig(ctx context.Context, sprigID string) (*GallerySprig, error) {
	var g GallerySprig
	var imp, upd int64
	err := s.db.QueryRowContext(ctx, "SELECT sprig_id, package_id, key_id, imported_at, updated_at FROM gallery_sprigs WHERE sprig_id = ?", sprigID).
		Scan(&g.SprigID, &g.PackageID, &g.KeyID, &imp, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.ImportedAt, g.UpdatedAt = time.Unix(imp, 0), time.Unix(upd, 0)
	return &g, nil
}

// PutGallerySprig records a gallery import, or an update of one (which
// keeps its first import time).
func (s *Store) PutGallerySprig(ctx context.Context, g GallerySprig) error {
	now := s.unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO gallery_sprigs (sprig_id, package_id, key_id, imported_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(sprig_id) DO UPDATE SET package_id = excluded.package_id, key_id = excluded.key_id, updated_at = excluded.updated_at`,
		g.SprigID, g.PackageID, g.KeyID, now, now)
	return err
}

// DeleteGallerySprig forgets a sprig's gallery record (an import undone).
func (s *Store) DeleteGallerySprig(ctx context.Context, sprigID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM gallery_sprigs WHERE sprig_id = ?", sprigID)
	return err
}

// GalleryRevokedKey is the setting holding revoked author key ids
// (comma or whitespace separated), until the gallery index brings its own
// revocation list.
const GalleryRevokedKey = "gallery.revoked_keys"
