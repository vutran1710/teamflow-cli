-- A project key belongs to the server and account that fetched it. Keys from before carry no binding, so no
-- sign-in could ever use them: they are dropped (whatever the old table holds) and fetched again when needed.
DROP TABLE IF EXISTS project_keys;
CREATE TABLE project_keys (
    server     TEXT NOT NULL,              -- the server URL, normalised (store.NormalizeServer)
    account    TEXT NOT NULL,              -- the signed-in account's email, lower-cased
    project    TEXT NOT NULL,
    key        TEXT NOT NULL,
    member     TEXT NOT NULL,
    org        TEXT NOT NULL,
    name       TEXT NOT NULL,
    fetched_at TEXT NOT NULL,
    PRIMARY KEY (server, account, project)
);
