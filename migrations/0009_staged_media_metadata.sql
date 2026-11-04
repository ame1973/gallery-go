-- Derived metadata is staged separately from the complete filesystem manifest.
-- Unsupported/corrupt media has an explicit safe diagnostic, not fabricated data.
ALTER TABLE inventory_generations ADD COLUMN metadata_count INTEGER NOT NULL DEFAULT 0 CHECK(metadata_count >= 0 AND metadata_count <= file_count);
ALTER TABLE inventory_generations ADD COLUMN metadata_complete INTEGER NOT NULL DEFAULT 0 CHECK(metadata_complete IN (0,1) AND (metadata_complete=0 OR metadata_count=file_count));
ALTER TABLE inventory_generations ADD COLUMN inspect_policy_sha256 TEXT NOT NULL DEFAULT '' CHECK(inspect_policy_sha256='' OR length(inspect_policy_sha256)=64);
CREATE TABLE inventory_media (
  generation_id TEXT NOT NULL,
  relative_path TEXT NOT NULL,
  inspect_version INTEGER NOT NULL CHECK(inspect_version > 0),
  status TEXT NOT NULL CHECK(status IN ('ok','unsupported','invalid','not_media')),
  kind TEXT NOT NULL CHECK(kind IN ('','image','video')),
  mime TEXT NOT NULL,
  inspection_json TEXT NOT NULL CHECK(json_valid(inspection_json)),
  PRIMARY KEY(generation_id,relative_path),
  FOREIGN KEY(generation_id,relative_path) REFERENCES inventory_files(generation_id,relative_path) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX idx_inventory_media_status ON inventory_media(generation_id,status,relative_path);
