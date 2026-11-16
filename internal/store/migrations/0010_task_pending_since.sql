-- When a task last became pending (DESIGN.md §6.3): set when it is created
-- and whenever it goes back to pending (a requeued lease, a job retry), so
-- the UI can show how long it has waited. Tasks pending before this column
-- existed are backfilled from their last ended lease, else their job's
-- creation; readers fall back to the job's creation for any NULL.
ALTER TABLE tasks ADD COLUMN pending_since INTEGER;
UPDATE tasks SET pending_since = COALESCE(
    (SELECT MAX(l.ended_at) FROM leases l WHERE l.job_id = tasks.job_id AND l.seq = tasks.seq),
    (SELECT j.created_at FROM jobs j WHERE j.id = tasks.job_id))
  WHERE state = 'pending';
