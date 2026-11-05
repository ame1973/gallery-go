-- Diagnostics are owned by job history, not the disposable source registration
-- or private inventory staging. Historical source/config/generation identifiers
-- deliberately have no live foreign key; removing a source never erases a cause.
CREATE TABLE job_issues (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  attempt INTEGER NOT NULL CHECK(attempt > 0),
  source_id TEXT NOT NULL,
  source_config_version INTEGER NOT NULL CHECK(source_config_version > 0),
  generation_id TEXT NOT NULL DEFAULT '',
  relative_path TEXT NOT NULL,
  error_code TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(job_id,attempt,source_id,generation_id,relative_path,error_code)
);
CREATE INDEX idx_job_issues_page ON job_issues(job_id,id);

INSERT INTO job_issues(id,job_id,attempt,source_id,source_config_version,generation_id,relative_path,error_code,created_at)
SELECT i.id,g.job_id,g.attempt,g.source_id,g.config_version,g.id,i.relative_path,i.error_code,i.created_at
FROM inventory_issues i JOIN inventory_generations g ON g.id=i.generation_id;

-- Existing fenced writers still co-commit staging, progress, events and audit.
-- Project only inserted issues so replayed immutable batches cannot duplicate it.
CREATE TRIGGER inventory_issue_history AFTER INSERT ON inventory_issues
BEGIN
  INSERT INTO job_issues(id,job_id,attempt,source_id,source_config_version,generation_id,relative_path,error_code,created_at)
  SELECT NEW.id,g.job_id,g.attempt,g.source_id,g.config_version,g.id,NEW.relative_path,NEW.error_code,NEW.created_at
  FROM inventory_generations g WHERE g.id=NEW.generation_id;
END;
