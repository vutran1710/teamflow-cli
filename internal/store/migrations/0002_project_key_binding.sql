-- A project key belongs to the server and account that fetched it. Rows from before carry no binding ('') and are
-- never used under a real config: the key is fetched again for the current sign-in.
CREATE TABLE project_keys_bound (
    server     TEXT NOT NULL,              -- the server URL, without a trailing slash
    account    TEXT NOT NULL,              -- the signed-in account's email, lower-cased
    project    TEXT NOT NULL,
    key        TEXT NOT NULL,
    member     TEXT NOT NULL,
    org        TEXT NOT NULL,
    name       TEXT NOT NULL,
    fetched_at TEXT NOT NULL,
    PRIMARY KEY (server, account, project)
);
INSERT INTO project_keys_bound (server, account, project, key, member, org, name, fetched_at)
    SELECT '', '', project, key, member, org, name, fetched_at FROM project_keys;
DROP TABLE project_keys;
ALTER TABLE project_keys_bound RENAME TO project_keys;
