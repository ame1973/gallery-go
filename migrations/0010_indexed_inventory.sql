-- Indexed resource versions are independent from private inventory/lease progress.
ALTER TABLE source_entries ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version>0);
ALTER TABLE source_entries ADD COLUMN file_identity TEXT NOT NULL DEFAULT '';
ALTER TABLE source_entries ADD COLUMN missing_since INTEGER;
ALTER TABLE source_entries ADD COLUMN missing_full_scans INTEGER NOT NULL DEFAULT 0 CHECK(missing_full_scans>=0);
CREATE INDEX idx_entry_fingerprint ON source_entries(source_id,sha256,size_bytes,relative_path);
ALTER TABLE media ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version>0);
ALTER TABLE media ADD COLUMN availability TEXT NOT NULL DEFAULT 'available' CHECK(availability IN ('pending_inventory','available','unavailable','quarantined','archived'));
ALTER TABLE media ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metadata_json));
CREATE UNIQUE INDEX uq_media_source_entry ON media(source_entry_id) WHERE source_entry_id IS NOT NULL;
ALTER TABLE people ADD COLUMN source_id TEXT REFERENCES sources(id) ON DELETE RESTRICT;
ALTER TABLE people ADD COLUMN relative_path TEXT;
ALTER TABLE people ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version>0);
ALTER TABLE people ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE people SET updated_at=created_at;
CREATE UNIQUE INDEX uq_person_source_path ON people(source_id,relative_path) WHERE source_id IS NOT NULL AND relative_path IS NOT NULL;
CREATE INDEX idx_album_person ON albums(person_id,id);
-- Preserve a deliberate cover choice even if its media temporarily moves away.
-- cover_media_id remains the current album-owned projection, never a foreign cover.
ALTER TABLE albums ADD COLUMN cover_override_id TEXT REFERENCES media(id) ON DELETE SET NULL;
UPDATE albums SET cover_override_id=cover_media_id WHERE EXISTS(SELECT 1 FROM media m WHERE m.id=albums.cover_media_id AND m.album_id=albums.id);
