-- StravaKudos state, schema version 1.
-- Migrations are applied by Store.Open and recorded in schema_version.

CREATE TABLE IF NOT EXISTS schema_version (
    v INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS auth (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    access_token TEXT    NOT NULL,
    obtained_at  INTEGER NOT NULL,
    expires_at   INTEGER  -- NULL if Strava doesn't tell us
);

CREATE TABLE IF NOT EXISTS athletes (
    id                 INTEGER PRIMARY KEY,
    display_name       TEXT    NOT NULL,
    relation           TEXT    NOT NULL
        CHECK (relation IN ('follower','following','both','cold')),
    added_at           INTEGER NOT NULL,
    last_seen_in_list  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS feed_checks (
    athlete_id         INTEGER PRIMARY KEY
        REFERENCES athletes(id) ON DELETE CASCADE,
    last_checked_at    INTEGER NOT NULL DEFAULT 0,
    last_status        TEXT,
    consecutive_errors INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_feed_checks_stale ON feed_checks(last_checked_at);

-- kudos_log is local audit + dedup cache. Authoritative source of
-- "already kudoed" is Strava's has_kudoed on the feed response;
-- we copy it into this table so we can skip POSTing without hitting
-- the feed again and to have a permanent audit trail.
--   api_status: 0  = bootstrap (observed has_kudoed=true, we did NOT POST)
--               201 = our POST succeeded
--               200 = POST returned OK (already kudoed, idempotent)
--               409 = Conflict (already kudoed, idempotent)
--               other = error
CREATE TABLE IF NOT EXISTS kudos_log (
    activity_id INTEGER PRIMARY KEY,
    athlete_id  INTEGER NOT NULL,
    kudoed_at   INTEGER NOT NULL,
    api_status  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_kudos_log_athlete ON kudos_log(athlete_id, kudoed_at);

CREATE TABLE IF NOT EXISTS api_calls (
    ts       INTEGER NOT NULL,
    endpoint TEXT    NOT NULL,
    status   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_api_calls_ts ON api_calls(ts);
