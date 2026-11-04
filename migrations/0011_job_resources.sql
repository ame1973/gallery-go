-- A job can depend on several sources without exposing its private target set.
-- Keys deliberately do not reference source rows: deletion must retain history.
CREATE TABLE job_resources (
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  resource_key TEXT NOT NULL CHECK(length(resource_key) BETWEEN 1 AND 256),
  PRIMARY KEY(job_id,resource_key)
) WITHOUT ROWID;
CREATE INDEX idx_job_resources_resource ON job_resources(resource_key,job_id);
INSERT INTO job_resources(job_id,resource_key)
SELECT id,resource_key FROM jobs WHERE resource_key IS NOT NULL AND resource_key!='';
