-- Popularity is the actual public retained-account view total, never a guessed score.
-- The same indexes serve reverse keyset scans for global and constrained selections.
CREATE INDEX idx_album_view_page ON albums(view_count,id);
CREATE INDEX idx_album_library_view_page ON albums(library_id,view_count,id);
CREATE INDEX idx_album_source_view_page ON albums(source_id,view_count,id);
CREATE INDEX idx_album_person_view_page ON albums(person_id,view_count,id);
