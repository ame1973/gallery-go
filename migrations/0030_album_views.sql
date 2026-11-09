-- Views are account-scoped, server-timed interactions; shared totals are derived.
ALTER TABLE albums ADD COLUMN view_count INTEGER NOT NULL DEFAULT 0
 CHECK(typeof(view_count)='integer' AND view_count>=0);

CREATE TABLE album_views (
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
 view_count INTEGER NOT NULL CHECK(typeof(view_count)='integer' AND view_count>=1),
 last_viewed_at INTEGER NOT NULL CHECK(typeof(last_viewed_at)='integer'),
 version INTEGER NOT NULL CHECK(typeof(version)='integer' AND version>=2),
 PRIMARY KEY(user_id,album_id)
);
CREATE INDEX idx_album_views_album ON album_views(album_id,user_id);

-- SQLite REPLACE may suppress DELETE triggers; reject duplicate inserts and use
-- explicit UPDATE for existing identities so a replacement cannot corrupt totals.
CREATE TRIGGER album_views_insert_guard BEFORE INSERT ON album_views
 WHEN EXISTS(SELECT 1 FROM album_views WHERE user_id=NEW.user_id AND album_id=NEW.album_id)
 BEGIN SELECT RAISE(ABORT,'album view identity already exists'); END;
CREATE TRIGGER album_views_identity_guard BEFORE UPDATE OF user_id,album_id ON album_views
 WHEN NEW.user_id IS NOT OLD.user_id OR NEW.album_id IS NOT OLD.album_id
 BEGIN SELECT RAISE(ABORT,'album view identity is immutable'); END;
CREATE TRIGGER album_views_add AFTER INSERT ON album_views BEGIN
 UPDATE albums SET view_count=view_count+NEW.view_count WHERE id=NEW.album_id;
END;
CREATE TRIGGER album_views_change AFTER UPDATE OF view_count ON album_views
 WHEN NEW.view_count!=OLD.view_count BEGIN
 UPDATE albums SET view_count=view_count+(NEW.view_count-OLD.view_count) WHERE id=NEW.album_id;
END;
CREATE TRIGGER album_views_remove AFTER DELETE ON album_views BEGIN
 UPDATE albums SET view_count=view_count-OLD.view_count WHERE id=OLD.album_id;
END;
