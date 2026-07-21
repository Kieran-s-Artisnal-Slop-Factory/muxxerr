-- Gateway database. Holds identity and which app instances each user has
-- provisioned. Never holds app data: every app instance keeps its own SQLite
-- file under data/instances/<user>/<app>/.

CREATE TABLE IF NOT EXISTS users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    username        TEXT    NOT NULL UNIQUE,
    -- Argon2id encoded hashes. Both are salted per-record and peppered with a
    -- server-side secret that lives outside the database (see internal/auth),
    -- so a stolen db file alone will not verify guesses offline.
    password_hash   TEXT    NOT NULL,
    passphrase_hash TEXT    NOT NULL,
    is_admin        INTEGER NOT NULL DEFAULT 0,
    is_disabled     INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL,
    -- Bumped whenever the password or passphrase changes; every existing
    -- session carries the value it was minted with, so a reset logs out all
    -- other devices without a sweep.
    credential_gen  INTEGER NOT NULL DEFAULT 1,
    last_login_at   TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    -- SHA-256 of the cookie token. The plaintext token never touches disk, so
    -- a database leak cannot be replayed as a live session.
    token_hash     TEXT    PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_gen INTEGER NOT NULL,
    created_at     TEXT    NOT NULL,
    expires_at     TEXT    NOT NULL,
    user_agent     TEXT,
    ip             TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- One row per (user, app) the user has chosen to run.
CREATE TABLE IF NOT EXISTS instances (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app          TEXT    NOT NULL,
    created_at   TEXT    NOT NULL,
    last_used_at TEXT,
    UNIQUE(user_id, app)
);

-- Admin-visible trail of security-relevant actions.
CREATE TABLE IF NOT EXISTS audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,
    actor      TEXT,
    action     TEXT NOT NULL,
    target     TEXT,
    detail     TEXT,
    ip         TEXT
);

CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at DESC);

-- Gateway settings an admin can change at runtime without editing apps.json.
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Failed-attempt counters for login and password reset, keyed by
-- "<kind>:<username>" and "<kind>:ip:<addr>".
CREATE TABLE IF NOT EXISTS throttle (
    key        TEXT    PRIMARY KEY,
    fails      INTEGER NOT NULL DEFAULT 0,
    locked_until TEXT,
    updated_at TEXT    NOT NULL
);
