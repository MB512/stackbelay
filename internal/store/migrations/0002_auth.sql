-- Users with a generated password (initial admin) must set their own
-- password before they can do anything else.
ALTER TABLE users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0;

CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_expires ON sessions(expires_at);
CREATE INDEX audit_log_at ON audit_log(at);
