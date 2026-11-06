-- Accepted tag-batch intent is metadata, never a worker/session capability.
-- Future processing must use these frozen targets and existing job lease fences.
CREATE TABLE album_tag_batches (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
 actor_user_id TEXT NOT NULL,
 tag_ids_json TEXT NOT NULL CHECK(json_valid(tag_ids_json) AND json_type(tag_ids_json)='array' AND json_array_length(tag_ids_json)<=100),
 created_at INTEGER NOT NULL
);
CREATE TABLE album_tag_batch_targets (
 job_id TEXT NOT NULL REFERENCES album_tag_batches(job_id) ON DELETE CASCADE,
 album_id TEXT NOT NULL,
 album_version INTEGER NOT NULL CHECK(album_version>0),
 library_id TEXT NOT NULL,
 source_id TEXT,
 source_config_version INTEGER,
 PRIMARY KEY(job_id,album_id),
 CHECK((source_id IS NULL AND source_config_version IS NULL) OR (source_id IS NOT NULL AND source_config_version IS NOT NULL AND source_config_version>0))
) WITHOUT ROWID;
CREATE TRIGGER album_tag_batch_creation_guard BEFORE INSERT ON album_tag_batches
 WHEN NOT EXISTS(SELECT 1 FROM jobs WHERE id=NEW.job_id AND kind='batch_tags' AND status='queued' AND attempts=0 AND version=1 AND cancel_requested=0 AND created_by_user_id=NEW.actor_user_id)
 BEGIN SELECT RAISE(ABORT,'batch intent requires its new authorized job'); END;
CREATE TRIGGER album_tag_batch_intent_immutable BEFORE UPDATE ON album_tag_batches
 BEGIN SELECT RAISE(ABORT,'batch intent is immutable'); END;
CREATE TRIGGER album_tag_batch_target_creation_guard BEFORE INSERT ON album_tag_batch_targets
 WHEN NOT EXISTS(SELECT 1 FROM jobs WHERE id=NEW.job_id AND kind='batch_tags' AND status='queued' AND attempts=0 AND version=1 AND cancel_requested=0)
 BEGIN SELECT RAISE(ABORT,'batch target requires its new job'); END;
CREATE TRIGGER album_tag_batch_target_immutable BEFORE UPDATE ON album_tag_batch_targets
 BEGIN SELECT RAISE(ABORT,'batch target snapshot is immutable'); END;
