-- Cover intent is metadata-only and retains historical IDs without media/source
-- foreign keys. Existing tag-batch intent, receipts and outcomes remain unchanged.
CREATE TABLE album_cover_batches (
  job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
  actor_user_id TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE album_cover_batch_targets (
  job_id TEXT NOT NULL REFERENCES album_cover_batches(job_id) ON DELETE CASCADE,
  album_id TEXT NOT NULL,
  album_version INTEGER NOT NULL CHECK(album_version>0),
  library_id TEXT NOT NULL,
  source_id TEXT,
  source_config_version INTEGER,
  requested_cover_id TEXT NOT NULL CHECK(length(requested_cover_id)<=64),
  cover_media_id TEXT,
  cover_media_version INTEGER,
  cover_revision_id TEXT,
  PRIMARY KEY(job_id,album_id),
  CHECK((source_id IS NULL AND source_config_version IS NULL) OR
    (source_id IS NOT NULL AND source_config_version IS NOT NULL AND source_config_version>0)),
  CHECK((cover_media_id IS NULL AND cover_media_version IS NULL AND cover_revision_id IS NULL) OR
    (cover_media_id IS NOT NULL AND cover_media_version IS NOT NULL AND cover_media_version>0)),
  CHECK(requested_cover_id='' OR (cover_media_id IS NOT NULL AND requested_cover_id=cover_media_id))
) WITHOUT ROWID;
CREATE TABLE album_cover_batch_runs (
  job_id TEXT NOT NULL REFERENCES album_cover_batches(job_id) ON DELETE CASCADE,
  attempt INTEGER NOT NULL CHECK(attempt BETWEEN 1 AND 100),
  target_count INTEGER NOT NULL CHECK(target_count BETWEEN 1 AND 100),
  created_at INTEGER NOT NULL,
  PRIMARY KEY(job_id,attempt),
  FOREIGN KEY(job_id,attempt) REFERENCES job_attempts(job_id,attempt) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE album_cover_batch_results (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  attempt INTEGER NOT NULL,
  album_id TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK(outcome IN ('applied','unchanged','already_applied','failed')),
  error_code TEXT CHECK(error_code IS NULL OR length(error_code) BETWEEN 1 AND 64),
  album_version INTEGER,
  created_at INTEGER NOT NULL,
  UNIQUE(job_id,attempt,album_id),
  FOREIGN KEY(job_id,attempt) REFERENCES album_cover_batch_runs(job_id,attempt) ON DELETE CASCADE,
  FOREIGN KEY(job_id,album_id) REFERENCES album_cover_batch_targets(job_id,album_id) ON DELETE CASCADE,
  CHECK((outcome='failed' AND error_code IS NOT NULL AND album_version IS NULL) OR
    (outcome!='failed' AND error_code IS NULL AND album_version IS NOT NULL AND album_version>0))
);
CREATE UNIQUE INDEX idx_album_cover_batch_applied ON album_cover_batch_results(job_id,album_id) WHERE outcome IN ('applied','unchanged');
CREATE INDEX idx_album_cover_batch_result_page ON album_cover_batch_results(job_id,id);
CREATE TRIGGER album_cover_batch_creation_guard BEFORE INSERT ON album_cover_batches
  WHEN NOT EXISTS(SELECT 1 FROM jobs WHERE id=NEW.job_id AND kind='batch_covers' AND status='queued' AND attempts=0 AND version=1 AND cancel_requested=0 AND created_by_user_id=NEW.actor_user_id)
  BEGIN SELECT RAISE(ABORT,'cover intent requires its new authorized job'); END;
CREATE TRIGGER album_cover_batch_intent_immutable BEFORE UPDATE ON album_cover_batches
  BEGIN SELECT RAISE(ABORT,'cover intent is immutable'); END;
CREATE TRIGGER album_cover_batch_target_creation_guard BEFORE INSERT ON album_cover_batch_targets
  WHEN NOT EXISTS(SELECT 1 FROM jobs WHERE id=NEW.job_id AND kind='batch_covers' AND status='queued' AND attempts=0 AND version=1 AND cancel_requested=0)
  BEGIN SELECT RAISE(ABORT,'cover target requires its new job'); END;
CREATE TRIGGER album_cover_batch_target_immutable BEFORE UPDATE ON album_cover_batch_targets
  BEGIN SELECT RAISE(ABORT,'cover target snapshot is immutable'); END;
CREATE TRIGGER album_cover_batch_run_guard BEFORE INSERT ON album_cover_batch_runs
  WHEN NEW.target_count!=(SELECT COUNT(*) FROM album_cover_batch_targets WHERE job_id=NEW.job_id) OR NOT EXISTS(SELECT 1 FROM jobs WHERE id=NEW.job_id AND kind='batch_covers' AND status='running' AND attempts=NEW.attempt AND cancel_requested=0 AND lease_expires_at>NEW.created_at)
  BEGIN SELECT RAISE(ABORT,'cover run requires a live attempt'); END;
CREATE TRIGGER album_cover_batch_run_immutable BEFORE UPDATE ON album_cover_batch_runs
  BEGIN SELECT RAISE(ABORT,'cover work budget is immutable'); END;
CREATE TRIGGER album_cover_batch_result_guard BEFORE INSERT ON album_cover_batch_results
  WHEN NOT EXISTS(SELECT 1 FROM jobs WHERE id=NEW.job_id AND kind='batch_covers' AND status='running' AND attempts=NEW.attempt AND cancel_requested=0 AND lease_expires_at>NEW.created_at)
  BEGIN SELECT RAISE(ABORT,'cover result requires a live attempt'); END;
CREATE TRIGGER album_cover_batch_result_immutable BEFORE UPDATE ON album_cover_batch_results
  BEGIN SELECT RAISE(ABORT,'cover result is immutable'); END;
