-- Source selection must retain album-ID order without sorting a whole catalog.
-- Library/person and reverse-tag ordering indexes already exist and are reused.
CREATE INDEX idx_album_source_page ON albums(source_id,id);
