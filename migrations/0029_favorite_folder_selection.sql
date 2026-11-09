-- Seek live favorites within one collection or the default without clear tombstones.
CREATE INDEX idx_favorites_live_collection_page ON favorites(user_id,collection_id,album_id) WHERE favorited=1;
