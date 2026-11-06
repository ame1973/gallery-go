-- Keep cleared personal votes as versioned tombstones to prevent revision reuse.
-- ratings is a leaf child table; foreign-key enforcement remains enabled.
CREATE TABLE ratings_next(
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
 rating INTEGER CHECK(rating IS NULL OR (typeof(rating)='integer' AND rating BETWEEN 1 AND 5)),
 updated_at INTEGER NOT NULL,
 version INTEGER NOT NULL DEFAULT 1 CHECK(typeof(version)='integer' AND version>=1),
 PRIMARY KEY(user_id,album_id)
);
INSERT INTO ratings_next(user_id,album_id,rating,updated_at,version)
 SELECT user_id,album_id,rating,updated_at,1 FROM ratings;
DROP TABLE ratings;
ALTER TABLE ratings_next RENAME TO ratings;
CREATE INDEX idx_ratings_album ON ratings(album_id,rating);
