-- The same actor-scoped receipt ledger covers account, group, and library POSTs.
-- password_hash is empty for commands without a password; credential-bearing
-- commands continue to store bcrypt hashes and compare them outside transactions.
ALTER TABLE admin_user_requests RENAME TO mutation_receipts;
DROP INDEX idx_admin_user_requests_expiry;
CREATE INDEX idx_mutation_receipts_expiry ON mutation_receipts(expires_at);
