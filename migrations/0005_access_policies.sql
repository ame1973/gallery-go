-- Restriction state is independent of its group edges. Removing the last group
-- must leave a restricted album closed, not turn it into a library-wide album.
ALTER TABLE permission_groups ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0);
ALTER TABLE libraries ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0);
ALTER TABLE libraries ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE libraries SET updated_at = created_at;
ALTER TABLE albums ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0);
ALTER TABLE albums ADD COLUMN access_restricted INTEGER NOT NULL DEFAULT 0 CHECK(access_restricted IN (0, 1));
UPDATE albums SET access_restricted = 1 WHERE EXISTS(SELECT 1 FROM album_access WHERE album_id = albums.id);
CREATE TRIGGER latch_album_restriction AFTER INSERT ON album_access
BEGIN
  UPDATE albums SET access_restricted = 1 WHERE id = NEW.album_id;
END;
CREATE INDEX idx_user_groups_group ON user_groups(group_id, user_id);
CREATE INDEX idx_library_access_group ON library_access(group_id, library_id);
CREATE INDEX idx_album_access_group ON album_access(group_id, album_id);
CREATE INDEX idx_ratings_album ON ratings(album_id, rating);
