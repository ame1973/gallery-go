-- Preserve personal revisions after clearing a favorite and enforce collection ownership.
-- NO ACTION forbids implicit unfiling; the existing single-statement user cascade
-- still removes both owned collections and favorites without nulling user_id.
CREATE UNIQUE INDEX uq_collection_owner_id ON favorite_collections(user_id,id);
CREATE TABLE favorites_next(
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
 collection_id TEXT,
 created_at INTEGER NOT NULL,
 favorited INTEGER NOT NULL DEFAULT 1 CHECK(typeof(favorited)='integer' AND favorited IN(0,1)),
 updated_at INTEGER NOT NULL DEFAULT 0,
 version INTEGER NOT NULL DEFAULT 1 CHECK(typeof(version)='integer' AND version>=1),
 CHECK(favorited=1 OR collection_id IS NULL),
 PRIMARY KEY(user_id,album_id),
 FOREIGN KEY(user_id,collection_id) REFERENCES favorite_collections(user_id,id) ON DELETE NO ACTION
);
INSERT INTO favorites_next(user_id,album_id,collection_id,created_at,favorited,updated_at,version)
 SELECT user_id,album_id,collection_id,created_at,1,created_at,1 FROM favorites;
DROP TABLE favorites;
ALTER TABLE favorites_next RENAME TO favorites;
CREATE INDEX idx_favorites_collection ON favorites(user_id,collection_id,album_id);
