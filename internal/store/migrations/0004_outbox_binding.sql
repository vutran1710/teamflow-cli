-- The outbox belongs to a sign-in and a project, not to a key: keys are dropped (logout, a refused key) and
-- minted again, and what was queued goes out under whatever key the same sign-in holds for that project next.
-- tries counts sends, fails the failures that count towards giving up on an item (see syncer.GiveUpFails).
ALTER TABLE spool ADD COLUMN server TEXT NOT NULL DEFAULT '';
ALTER TABLE spool ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE spool ADD COLUMN project TEXT NOT NULL DEFAULT '';
ALTER TABLE spool ADD COLUMN tries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE spool ADD COLUMN fails INTEGER NOT NULL DEFAULT 0;
ALTER TABLE spool ADD COLUMN first_fail_at TEXT;
ALTER TABLE spool ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
-- Rows from before: the project from the signal; the sign-in is adopted on open from the key they were queued with.
UPDATE spool SET project = COALESCE(json_extract(params, '$.signal.project'), '')
    WHERE json_valid(params) AND method = 'signal.emit';
CREATE INDEX spool_outbox ON spool (server, account, project, n);
