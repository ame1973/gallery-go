-- Inventories are private staged snapshots. Only a completed, healthy, fenced
-- generation may reconcile indexed rows; incomplete or superseded work remains
-- history and never authorizes source deletion or index purge.
CREATE TABLE inventory_generations (
  id TEXT PRIMARY KEY,
  source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  attempt INTEGER NOT NULL CHECK(attempt > 0),
  lease_owner TEXT NOT NULL,
  config_version INTEGER NOT NULL CHECK(config_version > 0),
  generation INTEGER NOT NULL CHECK(generation > 0),
  mode TEXT NOT NULL CHECK(mode IN ('full','incremental')),
  scope_path TEXT NOT NULL DEFAULT '.',
  state TEXT NOT NULL CHECK(state IN ('walking','ready','published','failed','cancelled','superseded')),
  healthy INTEGER NOT NULL DEFAULT 0 CHECK(healthy IN (0,1)),
  file_count INTEGER NOT NULL DEFAULT 0 CHECK(file_count >= 0),
  size_bytes INTEGER NOT NULL DEFAULT 0 CHECK(size_bytes >= 0),
  base_counts_json TEXT NOT NULL,
  error_code TEXT,
  version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  completed_at INTEGER,
  published_at INTEGER,
  UNIQUE(source_id,generation),
  UNIQUE(job_id,attempt,source_id)
);
CREATE UNIQUE INDEX uq_inventory_active_source ON inventory_generations(source_id) WHERE state IN ('walking','ready');
CREATE INDEX idx_inventory_job ON inventory_generations(job_id,attempt,source_id);
CREATE TABLE inventory_files (
  generation_id TEXT NOT NULL REFERENCES inventory_generations(id) ON DELETE CASCADE,
  relative_path TEXT NOT NULL,
  size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
  mtime_ns INTEGER NOT NULL,
  file_identity TEXT NOT NULL,
  quick_fingerprint TEXT NOT NULL CHECK(length(quick_fingerprint)=64),
  sha256 TEXT NOT NULL CHECK(length(sha256)=64),
  PRIMARY KEY(generation_id,relative_path)
) WITHOUT ROWID;
CREATE INDEX idx_inventory_file_hash ON inventory_files(generation_id,sha256,size_bytes,relative_path);
CREATE TABLE inventory_issues (
  id TEXT PRIMARY KEY,
  generation_id TEXT NOT NULL REFERENCES inventory_generations(id) ON DELETE CASCADE,
  relative_path TEXT NOT NULL,
  error_code TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(generation_id,relative_path,error_code)
);
