-- Per-worker work counters (DESIGN.md §4.3). One row per worker that has
-- ended a counted lease, updated in the transaction that ends the lease.
-- The row goes with its worker (pruning deletes it).
CREATE TABLE worker_stats (
  worker_id TEXT PRIMARY KEY REFERENCES workers ON DELETE CASCADE,
  prepare_done INTEGER NOT NULL DEFAULT 0,          -- prepare tasks completed
  transcribe_done INTEGER NOT NULL DEFAULT 0,       -- transcribe tasks completed
  jobs INTEGER NOT NULL DEFAULT 0,                  -- distinct jobs with a task completed by the worker
  audio_ms INTEGER NOT NULL DEFAULT 0,              -- audio of completed transcribe tasks
  prepare_compute_ms INTEGER NOT NULL DEFAULT 0,
  transcribe_compute_ms INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,              -- leases ended 'failed'
  expired INTEGER NOT NULL DEFAULT 0,               -- leases ended 'expired'
  first_task_at INTEGER,                            -- first / last task completion
  last_task_at INTEGER);

-- Backfill from what is still retained. Completed tasks live as long as
-- their job (job retention); a completion's time is its done lease's end
-- if that lease is still kept, else the job's completion (or last update).
-- 16000 is the canonical sample rate.
INSERT INTO worker_stats (worker_id, prepare_done, transcribe_done, jobs, audio_ms, prepare_compute_ms,
  transcribe_compute_ms, first_task_at, last_task_at)
SELECT d.worker_id,
  SUM(d.kind = 'prepare'),
  SUM(d.kind = 'transcribe'),
  COUNT(DISTINCT d.job_id),
  COALESCE(SUM(CASE WHEN d.kind = 'transcribe' THEN (d.end_sample - d.start_sample) * 1000 / 16000 END), 0),
  COALESCE(SUM(CASE WHEN d.kind = 'prepare' THEN d.compute_ms END), 0),
  COALESCE(SUM(CASE WHEN d.kind = 'transcribe' THEN d.compute_ms END), 0),
  MIN(d.at), MAX(d.at)
FROM (SELECT t.worker_id, t.job_id, t.kind, t.start_sample, t.end_sample, t.compute_ms,
        COALESCE((SELECT MAX(l.ended_at) FROM leases l
                  WHERE l.job_id = t.job_id AND l.seq = t.seq AND l.outcome = 'done'),
                 j.completed_at, j.updated_at) AS at
      FROM tasks t JOIN jobs j ON j.id = t.job_id
      WHERE t.state = 'done' AND t.worker_id IN (SELECT id FROM workers)) d
GROUP BY d.worker_id;

-- Failures and expiries are known only from ended leases, kept 30 days.
INSERT INTO worker_stats (worker_id)
SELECT DISTINCT worker_id FROM leases
WHERE outcome IN ('failed', 'expired') AND worker_id IN (SELECT id FROM workers)
ON CONFLICT (worker_id) DO NOTHING;
UPDATE worker_stats SET
  failures = (SELECT COUNT(*) FROM leases l WHERE l.worker_id = worker_stats.worker_id AND l.outcome = 'failed'),
  expired = (SELECT COUNT(*) FROM leases l WHERE l.worker_id = worker_stats.worker_id AND l.outcome = 'expired');
