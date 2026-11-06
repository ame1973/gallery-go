-- Preserve existing tag IDs, exact labels and album associations. Search text
-- normalization is a separate projection, never this catalog's label identity.
ALTER TABLE tags ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version>0);
CREATE INDEX idx_album_tag_reverse ON album_tags(tag_id,album_id);
