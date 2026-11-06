-- Reserve one truthful work budget per live attempt. Frozen intent/targets from
-- 0017 are unchanged; outcome history is not a worker authorization capability.
CREATE TABLE album_tag_batch_runs (
  job_id TEXT NOT NULL REFERENCES album_tag_batches(job_id) ON DELETE CASCADE,
  attempt INTEGER NOT NULL CHECK(attempt BETWEEN 1 AND 100),
  target_count INTEGER NOT NULL CHECK(target_count BETWEEN 1 AND 100),
  created_at INTEGER NOT NULL,
  PRIMARY KEY(job_id,attempt),
  FOREIGN KEY(job_id,attempt) REFERENCES job_attempts(job_id,attempt) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE album_tag_batch_results (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  attempt INTEGER NOT NULL,
  album_id TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK(outcome IN ('applied','unchanged','already_applied','failed')),
  error_code TEXT CHECK(error_code IS NULL OR length(error_code) BETWEEN 1 AND 64),
  album_version INTEGER,
  created_at INTEGER NOT NULL,
  UNIQUE(job_id,attempt,album_id),
  FOREIGN KEY(job_id,attempt) REFERENCES album_tag_batch_runs(job_id,attempt) ON DELETE CASCADE,
  FOREIGN KEY(job_id,album_id) REFERENCES album_tag_batch_targets(job_id,album_id) ON DELETE CASCADE,
  CHECK((outcome='failed' AND error_code IS NOT NULL AND album_version IS NULL) OR
    (outcome!='failed' AND error_code IS NULL AND album_version IS NOT NULL AND album_version>0))
);
-- A resolved target can never apply again, even after another metadata edit.
CREATE UNIQUE INDEX idx_album_tag_batch_applied ON album_tag_batch_results(job_id,album_id) WHERE outcome IN ('applied','unchanged');
CREATE INDEX idx_album_tag_batch_result_page ON album_tag_batch_results(job_id,id);
CREATE TRIGGER album_tag_batch_run_guard BEFORE INSERT ON album_tag_batch_runs
  WHEN NEW.target_count!=(SELECT COUNT(*) FROM album_tag_batch_targets WHERE job_id=NEW.job_id) OR NOT EXISTS(SELECT 1 FROM jobs WHERE id=NEW.job_id AND kind='batch_tags' AND status='running' AND attempts=NEW.attempt AND cancel_requested=0 AND lease_expires_at>NEW.created_at)
  BEGIN SELECT RAISE(ABORT,'batch run requires a live attempt'); END;
CREATE TRIGGER album_tag_batch_run_immutable BEFORE UPDATE ON album_tag_batch_runs
  BEGIN SELECT RAISE(ABORT,'batch work budget is immutable'); END;
CREATE TRIGGER album_tag_batch_result_guard BEFORE INSERT ON album_tag_batch_results
  WHEN NOT EXISTS(SELECT 1 FROM jobs WHERE id=NEW.job_id AND kind='batch_tags' AND status='running' AND attempts=NEW.attempt AND cancel_requested=0 AND lease_expires_at>NEW.created_at)
  BEGIN SELECT RAISE(ABORT,'batch result requires a live attempt'); END;
CREATE TRIGGER album_tag_batch_result_immutable BEFORE UPDATE ON album_tag_batch_results
  BEGIN SELECT RAISE(ABORT,'batch result is immutable'); END;
