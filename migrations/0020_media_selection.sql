-- Ordered media selection remains bounded to an album, optional kind and keyset.
-- The capture sentinel precedes every supported RFC3339 instant; it is not a date.
CREATE INDEX idx_media_album_kind ON media(album_id,kind,id);
CREATE INDEX idx_media_album_capture ON media(album_id,COALESCE(capture_at,-9223372036854775808),id);
CREATE INDEX idx_media_album_kind_capture ON media(album_id,kind,COALESCE(capture_at,-9223372036854775808),id);
