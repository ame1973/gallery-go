-- Scan selections are private immutable snapshots, not unbounded job JSON arrays.
-- Source/album IDs are retained after registration removal for safe job history.
CREATE TABLE scan_targets (
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL,
  source_config_version INTEGER NOT NULL CHECK(source_config_version>0),
  scope_path TEXT NOT NULL,
  album_id TEXT,
  PRIMARY KEY(job_id,source_id)
) WITHOUT ROWID;
