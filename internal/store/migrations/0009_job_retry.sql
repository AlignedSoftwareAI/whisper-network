-- Job retry (DESIGN.md §4.2): how many times a failed job was retried, and
-- on each lease the job's retry count when it was claimed. The retry-
-- elsewhere rule and a task's counted attempts only consider leases of the
-- current retry, so a retry starts with a fresh attempt budget and no
-- exclusions while the earlier leases stay for history.
ALTER TABLE jobs ADD COLUMN retries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE leases ADD COLUMN retry INTEGER NOT NULL DEFAULT 0;

-- The queue view's state order (DESIGN.md §6.1, ?sort=state): running, then
-- queued and preparing (oldest first), then failed, then done (most recently
-- finished first). The two expressions must match jobStateRankSQL and
-- jobStateKeySQL exactly for the planner to use the index.
CREATE INDEX jobs_queue_state_order ON jobs (queue_id,
  (CASE state WHEN 'running' THEN 0 WHEN 'queued' THEN 1 WHEN 'preparing' THEN 1 WHEN 'failed' THEN 2 ELSE 3 END),
  (CASE WHEN state IN ('failed','done') THEN -COALESCE(completed_at, created_at) ELSE created_at END),
  id);
