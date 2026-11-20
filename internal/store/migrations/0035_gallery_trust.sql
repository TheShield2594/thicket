-- Signed gallery packages (thicket export-sprig --for-gallery).
-- gallery_keys: author keys trusted on first use. The first package
-- imported from a key stores it here; the preview of any later package
-- shows whether its key is already known.
CREATE TABLE gallery_keys (
    key_id      TEXT    PRIMARY KEY,
    public_key  TEXT    NOT NULL,
    first_seen  INTEGER NOT NULL,
    first_sprig TEXT    NOT NULL DEFAULT '',
    trusted_by  INTEGER REFERENCES users (id) ON DELETE SET NULL
);
-- gallery_sprigs: which sprigs came from the gallery, and under which
-- author key. An update of one must be signed by the same key.
CREATE TABLE gallery_sprigs (
    sprig_id    TEXT    PRIMARY KEY,
    package_id  TEXT    NOT NULL,
    key_id      TEXT    NOT NULL,
    imported_at INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
