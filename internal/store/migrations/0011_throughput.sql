-- Fleet throughput (DESIGN.md §4.3, #87): audio transcribed per minute
-- and queue, counted as each transcribe task completes, in the transaction
-- that ends its lease. It is independent of the job, so deleting a job
-- does not change it. One row per (minute, queue) with work; the reaper
-- prunes rows past the throughput retention (§8.5), which bounds the table.
CREATE TABLE throughput (
  bucket INTEGER NOT NULL,                    -- Unix minute (ms / 60000) of the completions
  queue_id INTEGER NOT NULL,                  -- the job's queue (no foreign key: the work was done)
  audio_ms INTEGER NOT NULL DEFAULT 0,        -- audio of transcribe tasks completed
  tasks INTEGER NOT NULL DEFAULT 0,           -- transcribe tasks completed
  PRIMARY KEY (bucket, queue_id)) WITHOUT ROWID;

-- Backfill from what is still retained: transcribe tasks whose done lease
-- is kept, timed by that lease's end. The reaper prunes anything older
-- than the retention on its next pass. 16000 is the canonical sample rate.
INSERT INTO throughput (bucket, queue_id, audio_ms, tasks)
SELECT l.ended_at / 60000, j.queue_id, SUM((t.end_sample - t.start_sample) * 1000 / 16000), COUNT(*)
FROM leases l
JOIN tasks t ON t.job_id = l.job_id AND t.seq = l.seq
JOIN jobs j ON j.id = l.job_id
WHERE l.outcome = 'done' AND t.kind = 'transcribe' AND t.state = 'done'
GROUP BY l.ended_at / 60000, j.queue_id;
