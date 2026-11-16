-- The stage a failed lease's worker reported (DESIGN.md §8.2): 'pre-work',
-- 'input' or 'child'; NULL for older workers and for every other outcome.
-- A 'pre-work' failure happened before the worker touched the task's input,
-- so it is not counted as an attempt and does not keep the worker from the
-- task.
ALTER TABLE leases ADD COLUMN stage TEXT;
