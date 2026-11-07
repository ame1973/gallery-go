-- Preserve the public native AVG while making high-rating keysets indexable.
-- Triggers maintain only this derived value; business versions/events stay with
-- the existing authorized rating and account-deletion transactions.
ALTER TABLE albums ADD COLUMN rating_avg REAL
 CHECK(rating_avg IS NULL OR (typeof(rating_avg)='real' AND rating_avg BETWEEN 1 AND 5));
UPDATE albums SET rating_avg=(SELECT AVG(r.rating) FROM ratings r INDEXED BY idx_ratings_album WHERE r.album_id=albums.id);

CREATE TRIGGER tr_album_rating_insert AFTER INSERT ON ratings BEGIN
 UPDATE albums SET rating_avg=(SELECT AVG(r.rating) FROM ratings r INDEXED BY idx_ratings_album WHERE r.album_id=albums.id)
 WHERE id=NEW.album_id;
END;
CREATE TRIGGER tr_album_rating_update AFTER UPDATE OF rating,album_id ON ratings
 WHEN OLD.rating IS NOT NEW.rating OR OLD.album_id!=NEW.album_id BEGIN
 UPDATE albums SET rating_avg=(SELECT AVG(r.rating) FROM ratings r INDEXED BY idx_ratings_album WHERE r.album_id=albums.id)
 WHERE id IN(OLD.album_id,NEW.album_id);
END;
CREATE TRIGGER tr_album_rating_delete AFTER DELETE ON ratings WHEN OLD.rating IS NOT NULL BEGIN
 UPDATE albums SET rating_avg=(SELECT AVG(r.rating) FROM ratings r INDEXED BY idx_ratings_album WHERE r.album_id=albums.id)
 WHERE id=OLD.album_id;
END;

CREATE INDEX idx_album_rating_page ON albums(COALESCE(rating_avg,0),id);
CREATE INDEX idx_album_library_rating_page ON albums(library_id,COALESCE(rating_avg,0),id);
CREATE INDEX idx_album_source_rating_page ON albums(source_id,COALESCE(rating_avg,0),id);
CREATE INDEX idx_album_person_rating_page ON albums(person_id,COALESCE(rating_avg,0),id);
