
-- A user is created on first GitHub sign-in. No password column exists here,
-- and none ever should: identity is delegated to GitHub, so there is no
-- credential of the user's to leak.
CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    github_id  BIGINT      NOT NULL UNIQUE,
    login      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only the SHA-256 of a session token is stored. A database dump therefore
-- cannot be replayed as a set of live sessions.
CREATE TABLE sessions (
    token_hash BYTEA       PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE watch_rules (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind              TEXT        NOT NULL CHECK (kind IN ('train', 'operator', 'station')),
    target            TEXT        NOT NULL CHECK (length(btrim(target)) > 0),
    threshold_minutes INT         NOT NULL CHECK (threshold_minutes BETWEEN 1 AND 1440),
    webhook_url       TEXT        NOT NULL DEFAULT '',
    active            BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Re-submitting the same rule is idempotent rather than a duplicate
    -- subscription that would double every future alert.
    UNIQUE (user_id, kind, target, threshold_minutes)
);
CREATE INDEX watch_rules_active_idx ON watch_rules (active) WHERE active;

-- The core invariant of the whole service.
--
-- A train that is 20 minutes late is still 20 minutes late on the next poll, so
-- without this constraint every run would re-notify. It is a UNIQUE index and
-- not an application-level check on purpose: the poller may overlap with itself
-- when a run is slow, and only the database can make "have we already alerted?"
-- atomic with the insert. Inserts use ON CONFLICT DO NOTHING and count the rows
-- that actually landed.
--
-- The departure date is part of the key because the same train number runs
-- daily; without it, today's alert would suppress tomorrow's.
CREATE TABLE alerts (
    id             BIGSERIAL PRIMARY KEY,
    rule_id        BIGINT      NOT NULL REFERENCES watch_rules (id) ON DELETE CASCADE,
    user_id        BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    train_number   INT         NOT NULL,
    departure_date DATE        NOT NULL,
    operator       TEXT        NOT NULL DEFAULT '',
    delay_minutes  INT         NOT NULL,
    station        TEXT        NOT NULL DEFAULT '',
    webhook_status TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (rule_id, train_number, departure_date)
);
CREATE INDEX alerts_user_recent_idx ON alerts (user_id, created_at DESC);

-- One row per poll, so a silent failure is visible as a gap or a stored error
-- rather than as alerts that simply stopped arriving.
CREATE TABLE poll_runs (
    id              BIGSERIAL PRIMARY KEY,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    trains_seen     INT NOT NULL DEFAULT 0,
    rules_evaluated INT NOT NULL DEFAULT 0,
    alerts_created  INT NOT NULL DEFAULT 0,
    error           TEXT
);
