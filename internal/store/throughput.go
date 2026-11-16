package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Fleet throughput (table throughput, #87): the audio of transcribe tasks
// completed, per minute and queue. A completion is counted in the
// transaction that ends its lease (countCompletion) and is independent of
// the job, so deleting the job later does not change it. The reaper prunes
// buckets older than its retention, so the table holds at most one row per
// minute and queue of that retention.

// throughputBucket is the bucket width.
const throughputBucket = time.Minute

// bucketOf returns the bucket holding t.
func bucketOf(t time.Time) int64 { return toMS(t) / throughputBucket.Milliseconds() }

// countThroughput adds one completed transcribe task of audioMS to now's
// bucket of the queue.
func countThroughput(ctx context.Context, tx *sql.Tx, queueID, audioMS int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO throughput (bucket, queue_id, audio_ms, tasks) VALUES (?, ?, ?, 1)
		ON CONFLICT (bucket, queue_id) DO UPDATE SET audio_ms = audio_ms + excluded.audio_ms, tasks = tasks + 1`,
		bucketOf(now), queueID, audioMS); err != nil {
		return fmt.Errorf("throughput: %w", err)
	}
	return nil
}

// Throughput is the transcribe work completed over a window.
type Throughput struct {
	Audio time.Duration // audio transcribed
	Tasks int64         // transcribe tasks completed
}

// ThroughputSince returns the transcribe work completed at or after since,
// in total and by queue id. The window starts at since's bucket, so it can
// include up to one bucket (a minute) before since.
func (s *Store) ThroughputSince(ctx context.Context, since time.Time) (Throughput, map[int64]Throughput, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT queue_id, SUM(audio_ms), SUM(tasks) FROM throughput
		WHERE bucket >= ? GROUP BY queue_id`, bucketOf(since))
	if err != nil {
		return Throughput{}, nil, fmt.Errorf("throughput: %w", err)
	}
	defer rows.Close()
	var total Throughput
	byQueue := map[int64]Throughput{}
	for rows.Next() {
		var q, ms, n int64
		if err := rows.Scan(&q, &ms, &n); err != nil {
			return Throughput{}, nil, fmt.Errorf("throughput: %w", err)
		}
		tp := Throughput{Audio: time.Duration(ms) * time.Millisecond, Tasks: n}
		byQueue[q] = tp
		total.Audio += tp.Audio
		total.Tasks += tp.Tasks
	}
	return total, byQueue, rows.Err()
}

// PruneThroughput deletes the throughput buckets that end at or before
// cutoff and returns how many.
func (s *Store) PruneThroughput(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM throughput WHERE bucket < ?`, bucketOf(cutoff))
		if err != nil {
			return fmt.Errorf("prune throughput: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
