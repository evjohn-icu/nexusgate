-- 0023: WebDAV 交付账号
-- On-demand WebDAV spaces deliver footage to editing agents. Each account is
-- a Basic-Auth credential whose password is stored only as a bcrypt hash
-- (never plaintext). Spaces themselves are runtime state (session-scoped),
-- so only accounts are persisted here.
CREATE TABLE IF NOT EXISTS webdav_accounts (
    username      TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);
