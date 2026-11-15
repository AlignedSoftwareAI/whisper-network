-- Worker circuit breaker (DESIGN.md §4.2). A worker's current failure
-- streak: its leases ended failed (retryable) or expired since its last
-- completed task, one row per job with the failures in that job. A completed
-- task and a resume empty it. Rows go with their worker and their job.
CREATE TABLE worker_failure_streak (
  worker_id TEXT NOT NULL REFERENCES workers ON DELETE CASCADE,
  job_id TEXT NOT NULL REFERENCES jobs ON DELETE CASCADE,
  failures INTEGER NOT NULL,
  PRIMARY KEY (worker_id, job_id)) WITHOUT ROWID;

-- Why the server paused a worker by itself; NULL for any other state change.
ALTER TABLE workers ADD COLUMN pause_reason TEXT;
