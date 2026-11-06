-- Literal effective-title ordering, not search normalization or locale collation.
-- Keep this expression identical to the shared public title projection/comparator.
CREATE INDEX idx_album_title_page ON albums(COALESCE(NULLIF(display_name,''),source_name) COLLATE BINARY,id);
CREATE INDEX idx_album_library_title_page ON albums(library_id,COALESCE(NULLIF(display_name,''),source_name) COLLATE BINARY,id);
CREATE INDEX idx_album_source_title_page ON albums(source_id,COALESCE(NULLIF(display_name,''),source_name) COLLATE BINARY,id);
CREATE INDEX idx_album_person_title_page ON albums(person_id,COALESCE(NULLIF(display_name,''),source_name) COLLATE BINARY,id);
