-- The spool is the project's ordered outbox: besides signal.emit it holds the human acts drift.resolve and
-- drift.dismiss, each sent alone in its place. Rows from before are signal.emit.
ALTER TABLE spool ADD COLUMN method TEXT NOT NULL DEFAULT 'signal.emit';
