-- Chronological album keysets use immutable first-indexed server time, not NAS mtime.
-- Both tuple components reverse together; ID-only/reverse-tag indexes remain intact.
CREATE INDEX idx_album_indexed_page ON albums(created_at,id);
CREATE INDEX idx_album_library_indexed_page ON albums(library_id,created_at,id);
CREATE INDEX idx_album_source_indexed_page ON albums(source_id,created_at,id);
CREATE INDEX idx_album_person_indexed_page ON albums(person_id,created_at,id);
