-- alone: the item goes in a request of its own next time (the whole request it was in failed, so which call the
-- server failed on is not known; alone, a failure is its own).
ALTER TABLE spool ADD COLUMN alone INTEGER NOT NULL DEFAULT 0;
-- Drift acts queued before outboxes were bound carry no project: it is their drift's, when the replica knows it.
UPDATE spool SET project = COALESCE((SELECT d.project FROM drifts d WHERE d.id = json_extract(spool.params, '$.drift')), '')
    WHERE project = '' AND method IN ('drift.resolve', 'drift.dismiss') AND json_valid(params);
