-- Indexed-content duplicate lookup; historical revisions never become candidates
-- unless they are the current revision owned by their logical media row.
CREATE INDEX idx_revision_exact_signature ON media_revisions(source_sha256,source_size,media_id,id);
