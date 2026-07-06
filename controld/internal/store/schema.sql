-- schema.sql — controld state. Applied idempotently at startup (Init).
-- apps = desired state + current live container; deploys = full history,
-- one row per deploy attempt, status tells you which step it died in.

CREATE TABLE IF NOT EXISTS apps (
    name           text PRIMARY KEY,
    image          text NOT NULL,
    container_port int  NOT NULL CHECK (container_port BETWEEN 1 AND 65535),
    host           text NOT NULL,
    container_id   text NOT NULL DEFAULT '', -- '' = never successfully deployed
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- One host routes to exactly one app. Two apps claiming the same host is an
-- invalid state — first matching route wins at the edge and the other app
-- silently gets zero traffic — so make it unrepresentable here. A unique
-- index rather than a table constraint because Init re-applies this file to
-- already-created tables and ALTER TABLE ... ADD CONSTRAINT has no
-- IF NOT EXISTS. If this statement ever fails, two apps already share a
-- host — fix the rows; do not drop the index.
CREATE UNIQUE INDEX IF NOT EXISTS apps_host_key ON apps (host);

CREATE TABLE IF NOT EXISTS deploys (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_name    text NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
    image       text NOT NULL,
    status      text NOT NULL CHECK (status IN
                    ('pending','pulling','starting','routing','live','failed')),
    error       text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz -- set when status reaches live/failed
);

CREATE INDEX IF NOT EXISTS deploys_app_started_idx ON deploys (app_name, started_at DESC);
