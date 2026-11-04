-- Worker fencing and public job history are durable. No worker or queued task is
-- created by migration, and private lease/payload fields are never API capabilities.
ALTER TABLE jobs ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0);
ALTER TABLE jobs ADD COLUMN cancelled INTEGER NOT NULL DEFAULT 0 CHECK(cancelled >= 0);
ALTER TABLE jobs ADD COLUMN total_known INTEGER NOT NULL DEFAULT 0 CHECK(total_known IN (0,1));
ALTER TABLE jobs ADD COLUMN error_code TEXT;
ALTER TABLE jobs ADD COLUMN dedupe_token TEXT NOT NULL DEFAULT '';
UPDATE jobs SET dedupe_token = COALESCE(dedupe_key, '');
UPDATE jobs SET dedupe_key = NULL WHERE status IN ('succeeded','failed','cancelled','dead');
ALTER TABLE jobs ADD COLUMN created_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE jobs ADD COLUMN worker_id TEXT;
ALTER TABLE jobs ADD COLUMN started_at INTEGER;
ALTER TABLE jobs ADD COLUMN finished_at INTEGER;
ALTER TABLE job_attempts ADD COLUMN worker_id TEXT;
ALTER TABLE job_attempts ADD COLUMN base_counts_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE job_attempts ADD COLUMN counts_json TEXT NOT NULL DEFAULT '{}';
CREATE INDEX idx_jobs_expired_lease ON jobs(lease_expires_at,id) WHERE status='running';
CREATE INDEX idx_jobs_priority_due ON jobs(priority DESC,next_run_at,created_at,id) WHERE status='queued';
CREATE TABLE workers (
  id TEXT PRIMARY KEY,
  role TEXT NOT NULL CHECK(role IN ('serve','worker')),
  slots INTEGER NOT NULL CHECK(slots BETWEEN 1 AND 16),
  state TEXT NOT NULL CHECK(state IN ('running','stopping','stopped')),
  started_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);
CREATE INDEX idx_workers_seen ON workers(last_seen_at);
