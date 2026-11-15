-- Initial schema (DESIGN.md §4.3). Timestamps are Unix milliseconds, UTC.

CREATE TABLE queues (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  weight INTEGER NOT NULL DEFAULT 100 CHECK (weight BETWEEN 1 AND 10000),
  max_upload_bytes INTEGER,
  created_at INTEGER NOT NULL);

CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  must_change_password INTEGER NOT NULL DEFAULT 0,
  disabled_at INTEGER,
  created_at INTEGER NOT NULL);

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,                    -- sha256 of the cookie value
  user_id INTEGER NOT NULL REFERENCES users ON DELETE CASCADE,
  csrf TEXT NOT NULL,
  expires_at INTEGER NOT NULL);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE clients (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  token_hash TEXT UNIQUE NOT NULL,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at INTEGER);

CREATE TABLE workers (
  id TEXT PRIMARY KEY,
  token_hash TEXT UNIQUE NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('pending','active','paused','rejected')),
  hostname TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  cpu_model TEXT NOT NULL DEFAULT '',
  cores INTEGER NOT NULL DEFAULT 0,
  mem_bytes INTEGER NOT NULL DEFAULT 0,
  os TEXT NOT NULL DEFAULT '',
  arch TEXT NOT NULL DEFAULT '',
  engine_version TEXT NOT NULL DEFAULT '',
  ffmpeg_version TEXT NOT NULL DEFAULT '',
  sandbox_level TEXT NOT NULL DEFAULT '',
  kinds TEXT NOT NULL DEFAULT '[]',       -- JSON
  slots INTEGER NOT NULL DEFAULT 0,
  models TEXT NOT NULL DEFAULT '[]',      -- JSON
  version TEXT NOT NULL DEFAULT '',
  registered_at INTEGER NOT NULL,
  decided_by TEXT,
  decided_at INTEGER,
  last_seen_at INTEGER,
  gauges TEXT);                           -- JSON, latest heartbeat gauges
CREATE INDEX workers_state ON workers (state);

CREATE TABLE grants (
  principal TEXT NOT NULL,                -- 'user:<id>' | 'client:<id>' | 'worker:<id>'
  role TEXT NOT NULL,
  scope TEXT NOT NULL,                    -- '*' | 'queue:<id>' | 'worker:<id>'
  granted_by TEXT NOT NULL,
  granted_at INTEGER NOT NULL,
  PRIMARY KEY (principal, role, scope));
CREATE INDEX grants_role_scope ON grants (role, scope);

CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  queue_id INTEGER NOT NULL REFERENCES queues,
  client_id TEXT NOT NULL,
  idempotency_key TEXT,
  state TEXT NOT NULL CHECK (state IN ('preparing','queued','running','done','failed')),
  weight TEXT NOT NULL DEFAULT 'medium' CHECK (weight IN ('low','medium','high')),
  model TEXT NOT NULL,
  language TEXT NOT NULL,
  prompt TEXT NOT NULL DEFAULT '',
  label TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  metadata TEXT,
  original_bytes INTEGER NOT NULL DEFAULT 0,
  original_sha256 TEXT NOT NULL DEFAULT '',
  original_type TEXT NOT NULL DEFAULT '',
  original_purged_at INTEGER,
  canonical_bytes INTEGER,
  canonical_sha256 TEXT,
  audio_samples INTEGER,
  canonical_purged_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  started_at INTEGER,
  completed_at INTEGER,                   -- set on done and on failed
  compute_ms INTEGER,
  error TEXT,
  first_downloaded_at INTEGER,
  last_downloaded_at INTEGER,
  download_count INTEGER NOT NULL DEFAULT 0,
  UNIQUE (client_id, idempotency_key));
CREATE INDEX jobs_queue_state ON jobs (queue_id, state, created_at);
CREATE INDEX jobs_queue_created ON jobs (queue_id, created_at, id);
CREATE INDEX jobs_completed ON jobs (completed_at) WHERE completed_at IS NOT NULL;

CREATE TABLE tasks (
  job_id TEXT NOT NULL REFERENCES jobs ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('prepare','transcribe')),
  start_sample INTEGER,                   -- NULL for prepare
  end_sample INTEGER,
  cost REAL NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('pending','leased','done','failed','cancelled')),
  attempts INTEGER NOT NULL DEFAULT 0,
  result TEXT,
  engine_version TEXT,                    -- whisper.cpp for transcribe, ffmpeg for prepare
  worker_id TEXT,
  compute_ms INTEGER,
  error TEXT,
  PRIMARY KEY (job_id, seq));
CREATE INDEX tasks_pending ON tasks (job_id, seq) WHERE state = 'pending';
CREATE INDEX tasks_state ON tasks (state);

-- Leases are not foreign-keyed to tasks: a lease row outlives its job so that
-- a worker holding it gets "gone" rather than "unknown" after a delete. Ended
-- leases are removed by retention.
CREATE TABLE leases (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  worker_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ended_at INTEGER,
  outcome TEXT CHECK (outcome IN ('done','expired','released','failed','cancelled')));
CREATE INDEX leases_live ON leases (expires_at) WHERE ended_at IS NULL;
-- The invariant: at most one live lease per task.
CREATE UNIQUE INDEX leases_one_live ON leases (job_id, seq) WHERE ended_at IS NULL;
CREATE INDEX leases_worker_live ON leases (worker_id) WHERE ended_at IS NULL;
CREATE INDEX leases_ended ON leases (ended_at) WHERE ended_at IS NOT NULL;

CREATE TABLE sched_vtime (
  queue_id INTEGER NOT NULL REFERENCES queues ON DELETE CASCADE,
  class TEXT NOT NULL CHECK (class IN ('','low','medium','high')),  -- '' = queue level
  vtime REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (queue_id, class));

-- Last charged virtual time, for reactivation when nothing is backlogged.
-- scope 0 = queue level; scope <queue id> = class level within that queue.
CREATE TABLE sched_last (
  scope INTEGER PRIMARY KEY,
  vtime REAL NOT NULL);

CREATE TABLE audit (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '');
