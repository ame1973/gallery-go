-- Authentication state is private to this installation; no default user is created.
ALTER TABLE users ADD COLUMN scope_id TEXT NOT NULL DEFAULT '';
UPDATE users SET scope_id = lower(hex(randomblob(32)));
CREATE UNIQUE INDEX uq_users_scope ON users(scope_id);
ALTER TABLE users ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0);
ALTER TABLE users ADD COLUMN download_original INTEGER NOT NULL DEFAULT 0 CHECK(download_original IN (0, 1));

ALTER TABLE sessions ADD COLUMN id TEXT NOT NULL DEFAULT '';
UPDATE sessions SET id = lower(hex(randomblob(16)));
CREATE UNIQUE INDEX uq_sessions_id ON sessions(id);
ALTER TABLE sessions ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN ip_address TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_sessions_active ON sessions(user_id, id) WHERE revoked_at IS NULL;

CREATE TABLE installation (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  bootstrapped_at INTEGER NOT NULL
);
CREATE TABLE auth_rate_limits (
  bucket TEXT PRIMARY KEY,
  attempts INTEGER NOT NULL CHECK(attempts >= 0),
  window_started_at INTEGER NOT NULL
);
CREATE INDEX idx_auth_rate_window ON auth_rate_limits(window_started_at);
ALTER TABLE change_log ADD COLUMN actor_scope TEXT NOT NULL DEFAULT 'system';
CREATE INDEX idx_audit_actor_time ON audit_events(actor_user_id, created_at, id);
