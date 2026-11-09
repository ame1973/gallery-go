-- Keep live account favorites in album-ID order without scanning clear tombstones.
CREATE INDEX idx_favorites_active_page ON favorites(user_id,album_id) WHERE favorited=1;
