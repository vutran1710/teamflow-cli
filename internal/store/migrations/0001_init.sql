-- Teamflow client store (one per home folder; the server never sees it). Mode 0600, in a 0700 folder.

-- Replica of the member's projects, as synced.
CREATE TABLE projects (
    id   TEXT PRIMARY KEY,
    data TEXT NOT NULL                     -- Project JSON
);

CREATE TABLE topics (
    id      TEXT PRIMARY KEY,
    project TEXT NOT NULL,
    data    TEXT NOT NULL                  -- Topic JSON
);
CREATE INDEX topics_project ON topics(project);

CREATE TABLE states (
    project TEXT NOT NULL,
    version INTEGER NOT NULL,
    data    TEXT NOT NULL,                 -- ProjectState JSON
    PRIMARY KEY (project, version)
);

CREATE TABLE signals (
    id            TEXT PRIMARY KEY,
    project       TEXT NOT NULL,
    topic         TEXT NOT NULL,
    kind          TEXT NOT NULL,
    seq           INTEGER NOT NULL,
    chain         TEXT NOT NULL,
    chain_version INTEGER NOT NULL,
    state         TEXT NOT NULL,
    changed_seq   INTEGER NOT NULL,        -- seq of the last event that changed it (accepted or state change)
    data          TEXT NOT NULL            -- StoredSignal JSON
);
CREATE INDEX signals_project_seq ON signals(project, seq);
CREATE INDEX signals_project_changed ON signals(project, changed_seq);
CREATE INDEX signals_chain ON signals(chain, chain_version);
CREATE INDEX signals_topic ON signals(topic);
CREATE INDEX signals_state ON signals(state);

CREATE TABLE drifts (
    id      TEXT PRIMARY KEY,
    project TEXT NOT NULL,
    status  TEXT NOT NULL,
    seq     INTEGER NOT NULL,              -- seq of the last event about it
    data    TEXT NOT NULL                  -- Drift JSON
);
CREATE INDEX drifts_project ON drifts(project, seq);

-- People in the projects of the replica: name, alias (one per person) and org title.
CREATE TABLE members (
    id    TEXT PRIMARY KEY,
    name  TEXT NOT NULL,
    alias TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT ''
);

-- Last seq synced, per project.
CREATE TABLE cursors (
    project TEXT PRIMARY KEY,
    seq     INTEGER NOT NULL
);

-- When the background sync last heard from a project's live socket (hooks skip their pull while it is fresh).
CREATE TABLE live (
    project TEXT PRIMARY KEY,
    at      TEXT NOT NULL
);

-- Small facts about the server: its protocol, and whether it refused this client (UPGRADE_REQUIRED).
CREATE TABLE meta (
    name  TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Emitted signals not yet accepted, in order. key_ref names the key they go out with.
CREATE TABLE spool (
    n          INTEGER PRIMARY KEY AUTOINCREMENT,
    signal     TEXT NOT NULL UNIQUE,
    session    TEXT NOT NULL,
    key_ref    TEXT NOT NULL,
    params     TEXT NOT NULL,              -- SignalEmit JSON
    created_at TEXT NOT NULL
);

-- Agent sessions on this machine.
CREATE TABLE sessions (
    id            TEXT PRIMARY KEY,
    agent         TEXT NOT NULL,
    agent_pid     INTEGER,                 -- the agent process; its hooks and MCP server are its descendants
    cwd           TEXT NOT NULL DEFAULT '',
    project       TEXT,                    -- set while the session is shared
    member        TEXT,                    -- the member the session acts as
    key_ref       TEXT,
    topics        TEXT,                    -- JSON list of the topics followed; NULL: all of them
    last_seq      INTEGER NOT NULL DEFAULT 0,   -- last project event injected into the session
    state_version INTEGER NOT NULL DEFAULT 0,   -- last plan/instruction version injected
    started_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE INDEX sessions_pid ON sessions(agent_pid, updated_at);
CREATE INDEX sessions_project ON sessions(project);

CREATE TABLE citations (
    session  TEXT NOT NULL,
    signal   TEXT NOT NULL,
    cited_at TEXT NOT NULL,
    PRIMARY KEY (session, signal)
);
CREATE INDEX citations_signal ON citations(signal);

-- One notice per (session, type, about); a notice about nothing (NULL) is never folded.
CREATE TABLE notices (
    n          INTEGER PRIMARY KEY AUTOINCREMENT,
    session    TEXT NOT NULL,
    type       TEXT NOT NULL,
    about      TEXT,
    text       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    shown      INTEGER NOT NULL DEFAULT 0,
    UNIQUE (session, type, about)
);

-- Artifacts the project's initiator archived (by chain id), from artifact.archived events.
CREATE TABLE artifact_archive (
    chain    TEXT PRIMARY KEY,
    project  TEXT NOT NULL,
    archived INTEGER NOT NULL,
    note     TEXT NOT NULL DEFAULT ''
);

-- Project keys this machine fetched (one per project, with the account's auth key).
CREATE TABLE project_keys (
    project    TEXT PRIMARY KEY,
    key        TEXT NOT NULL,
    member     TEXT NOT NULL,
    org        TEXT NOT NULL,
    name       TEXT NOT NULL,
    fetched_at TEXT NOT NULL
);

-- A verdict the person typed themselves (their own prompt); a verdict command needs a fresh one and uses it up.
CREATE TABLE verdict_grants (
    n          INTEGER PRIMARY KEY,
    session    TEXT NOT NULL,
    signal     TEXT NOT NULL,
    kind       TEXT NOT NULL,              -- APPROVE or REJECT
    created_at TEXT NOT NULL,
    used_at    TEXT
);

-- A project's Deliverables as last published: version, archived, the seq of the event that carried it, the JSON.
CREATE TABLE deliverables (
    project  TEXT PRIMARY KEY,
    version  INTEGER NOT NULL,
    archived INTEGER NOT NULL DEFAULT 0,
    seq      INTEGER NOT NULL DEFAULT 0,
    data     TEXT NOT NULL
);
