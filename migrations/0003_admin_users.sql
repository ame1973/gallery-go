-- Idempotent administrative POSTs retain only bcrypt password hashes, never raw
-- passwords or fast password fingerprints. Results expire after 24 hours.
CREATE TABLE admin_user_requests (
  actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  request_key TEXT NOT NULL,
  operation TEXT NOT NULL,
  request_json TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY(actor_user_id, request_key)
);
CREATE INDEX idx_admin_user_requests_expiry ON admin_user_requests(expires_at);
CREATE INDEX idx_users_active_admin ON users(id) WHERE role = 'admin' AND active = 1;
CREATE INDEX idx_playlists_user ON playlists(user_id);
CREATE INDEX idx_playback_progress_playlist ON playback_progress(playlist_id);
CREATE INDEX idx_sync_operations_user ON sync_operations(user_id);
