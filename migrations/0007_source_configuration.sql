-- Source configuration is independently versioned from reachability checks.
-- Roots remain private and can only be selected from the runtime YAML registry.
ALTER TABLE sources ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0);
ALTER TABLE sources ADD COLUMN config_version INTEGER NOT NULL DEFAULT 1 CHECK(config_version > 0);
ALTER TABLE sources ADD COLUMN is_active INTEGER NOT NULL DEFAULT 1 CHECK(is_active IN (0,1));
ALTER TABLE sources ADD COLUMN minimum_files INTEGER NOT NULL DEFAULT 0 CHECK(minimum_files BETWEEN 0 AND 1000000000);
ALTER TABLE sources ADD COLUMN poll_interval_seconds INTEGER NOT NULL DEFAULT 60 CHECK(poll_interval_seconds BETWEEN 10 AND 86400);
ALTER TABLE sources ADD COLUMN full_scan_interval_seconds INTEGER NOT NULL DEFAULT 86400 CHECK(full_scan_interval_seconds BETWEEN 10 AND 604800 AND full_scan_interval_seconds >= poll_interval_seconds);
ALTER TABLE sources ADD COLUMN root_config_sha256 TEXT NOT NULL DEFAULT '' CHECK(length(root_config_sha256) IN (0,64));
ALTER TABLE sources ADD COLUMN last_checked_at INTEGER;
ALTER TABLE sources ADD COLUMN last_error_code TEXT;
ALTER TABLE sources ADD COLUMN last_full_scan_at INTEGER;
ALTER TABLE sources ADD COLUMN next_scan_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sources ADD COLUMN next_full_scan_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sources ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE sources SET updated_at=created_at;
-- An old inventory must not authorize original paths under a changed root/policy.
ALTER TABLE source_entries ADD COLUMN source_config_version INTEGER NOT NULL DEFAULT 0 CHECK(source_config_version >= 0);
CREATE INDEX idx_sources_due ON sources(is_active,next_scan_at,id);
CREATE INDEX idx_jobs_resource_active ON jobs(resource_key,id) WHERE status IN ('queued','running') AND cancel_requested=0;
