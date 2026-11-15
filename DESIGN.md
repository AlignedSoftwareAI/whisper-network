# whisper-network — Design Spec

Status: draft r3, 2026-11-15

Changes in r2:
- Weighted fair scheduling across queues, plus per-job `low`/`medium`/`high` weight (§8.2).
- Any input format is accepted; the system normalizes it, not the client (§8.1, §7.4).
- FLAC support checked against the source (§8.1.1).
- Enrollment tokens deferred; explicit approval only.
- New observability section: JSONL logs for VictoriaLogs, metrics for VictoriaMetrics/Prometheus (§10).

Changes in r3:
- Canonical audio is always FLAC; the server encodes WAV uploads in pure Go (corrects §8.1.1).
- Built on the `golang-program` template: kong CLI, zerolog logging, Makefile and GitHub workflows. Go 1.27.

---

## 1. Purpose

Distribute Whisper transcription across an ad-hoc fleet of machines. Any machine that can run a container (or a static binary) can join as a worker, with nothing configured in advance except the server URL.

The motivating workload is a large backlog of recordings. Hundreds of hours of audio take days of CPU on a single machine, and the fleet turns that into hours. Clients are typically automation, such as a batch script or a pipeline step, that only needs to speak HTTP. Clients don't change when the fleet grows.

### Goals

- One Go binary with two modes: `server` and `worker`.
- Zero-config worker start: `whisper-network worker --server https://…`.
- No shared filesystem. Media and results move over HTTP.
- **The system absorbs the media work.** Clients upload whatever they have, and conversion happens inside the service.
- At-least-once processing with fenced leases. A crashed or partitioned worker never loses a job, and it can never overwrite a result it no longer owns.
- Weighted fair sharing of the fleet across queues, and across job weights within a queue.
- Per-queue trust boundaries. A worker sees media only from queues it has been granted.
- Transcription never competes with a machine's real work (lowest CPU and I/O priority).
- First-class observability: JSONL logs for VictoriaLogs, Prometheus-format metrics both scrapeable and pushable.
- SQLite metadata plus a local blob directory. One volume to back up.

### Non-goals (v1)

- High availability or multiple server replicas. One server, one SQLite file.
- Speaker diarization, translation, or any engine other than whisper.cpp.
- Fetching media from URLs. The service never fetches anything a client names.
- Push notifications to clients. Clients poll.
- **Enrollment tokens.** Every worker is approved explicitly by a human. Deferred, not planned.
- Multi-tenant hostile isolation between queue admins. Queue admins are trusted operators.

---

## 2. Decisions that differ from the base idea

Each of these changes something in the original idea. Confidence is given for each.

| # | Original idea | Spec does | Why | Confidence |
|---|---|---|---|---|
| D1 | Worker `GET /job` to take a job | `POST /worker/{id}/leases` (long-poll) | GET must be safe and idempotent. Proxies, retries and prefetchers repeat GETs, and a repeated claim leaks a lease. Claiming creates a resource (a lease), so POST is the right method. | High |
| D2 | Heartbeat and results on `/job/{id}/…` | On `/lease/{id}/…` | The lease ID doubles as a fencing token. A worker whose lease expired gets `410 Gone` on heartbeat or result, so a slow worker can't clobber a re-run. Workers never need to know the job/task split. | High |
| D3 | `POST /job/{id}` or `PUT …/results`? | `PUT /lease/{id}/result` | A result has exactly one value per lease, and the worker may retry the upload after a network error. PUT gives idempotent replace semantics: the same lease plus the same body returns 200 again. | High |
| D4 | One job = one unit of work | A job is split into **tasks**: one optional `prepare` task, then N `transcribe` tasks of ≤ ~1 h audio each. Workers lease tasks. | Chunking is already required for memory (4–5 h meetings OOM'd at 3 GB unchunked). Splitting on the server also lets one long meeting run in parallel across the fleet, and it gives media conversion a natural home (D5). | Medium |
| D5 | (unspecified) | **Accept any media.** Normalization (ffmpeg → 16 kHz mono FLAC) is a `prepare` task run by a worker in a sandbox. Uploads that are already canonical skip it. | Clients don't have to convert. ffmpeg on untrusted media runs in a sandboxed worker child rather than on the server, which holds every secret and the DB. | Medium-high |
| D6 | "One job per CPU" | Slots = min(cores ÷ threads-per-slot, memory budget ÷ model peak RSS). Defaults to 4 threads per slot. | See §7.3. One thread per job makes per-job latency ~4× worse and multiplies memory by the core count. | Medium (needs measurement; a `bench` subcommand is part of v1) |
| D7 | Default `admin` login | `admin` is created on first start with a random password, printed once to the log (or taken from `WHISPER_ADMIN_PASSWORD`), and must be changed at first login | A known default password on a network service is the most common way these get owned. | High |
| D8 | Worker has a UUID | The **server** assigns the UUID on `POST /worker` and returns it with a secret | POST-to-collection semantics. The server owns identity, and a client-chosen ID can't collide with or squat on another worker's. | High |
| D9 | "First untaken across all queues" | Two-level weighted fair queueing: by queue weight (admin-set), then by job weight class within a queue, then FIFO (§8.2) | Strict FIFO lets one backlog starve every other queue for days. | High on the need, medium on parameter defaults |

---

## 3. Architecture

```
            ┌──────────────────────────── server ─────────────────────────────┐
 clients ──▶│ /queue  /job  /client    ─┐                                     │
 (API token)│                           ├─▶ authz ─▶ store (SQLite, WAL)       │
 browsers ─▶│ /ui  (session cookie)    ─┤                │                    │
            │                           │   blobs/ (original, canonical FLAC, │
 workers ──▶│ /worker /lease /models   ─┘           transcripts)              │
 (worker    │   scheduler: weighted fair pick in the claim txn                │
  token)    │   dispatcher: wakes long-polls when tasks appear                │
            │   reaper: expires leases, purges jobs/media                     │
            │   obs: zerolog JSONL → stdout; /metrics (+ optional push)        │
            └────────────────────────────────────────────────────────────────┘

 worker process (one per machine)
   ├─ N slot goroutines: claim ─▶ download ─▶ sandbox-exec {ffmpeg | whisper-cli} ─▶ PUT result
   ├─ heartbeat per active lease (carries worker gauges to the server)
   ├─ model cache (volume), fetched from server /models/{name}
   └─ JSONL logs → stdout; optional metrics push/scrape
```

### Components

| Component | Responsibility |
|---|---|
| `store` | All SQLite access. One writer connection plus a read pool, WAL mode, `busy_timeout`. Claims run in one `BEGIN IMMEDIATE` transaction. |
| `blobs` | Content stored on disk under `data/blobs/{job-id}/`. The DB holds paths, sizes and sha256 hashes, never bytes. |
| `media` | Pure Go: WAV/FLAC validation and decode (`mewkiz/flac`), energy analysis, split-point selection, WAV range serving. No cgo, no ffmpeg on the server. |
| `sched` | Virtual-time bookkeeping and candidate selection (§8.2). Pure functions over a snapshot, so it is unit-testable without SQLite. |
| `dispatcher` | Wakes waiting long-poll claims when a task becomes available. In-memory only; correctness never depends on it. |
| `reaper` | Periodic job: expires leases, requeues or fails tasks, purges media and jobs past retention, deletes stale pending workers. |
| `auth` | Password, session, client-token and worker-token authentication. One `Can(principal, action, resource)` function. |
| `ui` | Server-rendered `html/template` pages with htmx for small interactions. No JS build step. |
| `obs` | zerolog setup (VictoriaLogs field names), event names, metrics registry, push loop. |
| `worker` | Slot loop, lease heartbeats, `sandbox-exec` supervision, model cache. |

### Why exec `whisper-cli` and `ffmpeg` instead of cgo bindings

- If a child crashes or is OOM-killed, the worker survives and reports a failure.
- Priority, memory limits, thread count and sandbox are set per child process.
- Engine versions are pinned in the image and reported in provenance, separate from the Go release cadence.
- The Go binary stays pure Go. `modernc.org/sqlite` keeps it cgo-free, so it cross-compiles and the server image is static.

Trade-off: the worker image ships three binaries (`whisper-network`, `whisper-cli`, `ffmpeg`), which bends "one program" slightly. The Go binary is still the only thing anyone runs.

---

## 4. Domain model

### 4.1 Entities

| Entity | Identity | Notes |
|---|---|---|
| **Queue** | human-readable name, `[a-z0-9][a-z0-9-]{0,62}` | `public` is created at bootstrap. Has a `weight` (admin-set). Can be renamed but not deleted while it has jobs. |
| **Job** | UUIDv7 | Client-facing. Owns the media, parameters, `weight` class, metadata, the assembled transcript and the download log. |
| **Task** | `{job}/{seq}` | Internal. `kind` is `prepare` (seq 0, optional) or `transcribe` (seq 1…N, a sample range of the canonical audio). |
| **Lease** | random 128-bit ID | One attempt by one worker at one task. Expires unless heartbeated. Fencing token. |
| **Worker** | server-assigned UUID | Registered machine. States: pending, active, paused, rejected. |
| **User** | username | Human UI login. |
| **Client** | ID plus bearer token | API principal. Created by a user in the UI or via `POST /client`. |
| **Grant** | (principal, role, scope) | All authorization data lives here. |

### 4.2 State machines

**Worker**

```
            approve                 pause
 pending ───────────▶ active ◀───────────▶ paused
    │                   │       resume        │
    └──── reject ───────┴──── reject ─────────┴──▶ rejected (token revoked)
```

- **pending**: may only `GET /worker/{id}` and wait. Claims return 403 with `state: pending`.
- **paused**: running leases continue to completion. New claims return 409 `paused`. The worker keeps polling slowly so it notices when it is resumed.
- **rejected**: the token is revoked and active leases are cancelled; the next heartbeat returns 410, and the worker kills its child and exits non-zero. Terminal. A new registration is a new worker.

**Job** (derived from its tasks, but stored for cheap querying)

```
 preparing ──▶ queued ──▶ running ──▶ done
     │            │          │
     └────────────┴──────────┴──▶ failed     (a task exhausted its attempts; sibling tasks cancelled)
 any state ──DELETE──▶ (row and blobs removed; active leases cancelled)
```

`preparing` is skipped when the upload is already canonical. Transcribe tasks are created when the canonical audio is accepted.

**Task**

```
 pending ──claim──▶ leased ──result──▶ done
    ▲                 │
    └── expired/released/failed (attempts < max) ──┘
                      └── failed (attempts == max) ──▶ failed
```

`attempts` increments when a lease is created. The default `max_attempts` is 2: a task that has started twice without finishing is abandoned rather than retried forever. A lease that times out counts as an attempt. A lease the worker releases voluntarily on shutdown does **not** count.

### 4.3 Schema (SQLite)

```sql
CREATE TABLE queues (
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, description TEXT,
  weight INTEGER NOT NULL DEFAULT 100 CHECK (weight BETWEEN 1 AND 10000),
  max_upload_bytes INTEGER, created_at INTEGER NOT NULL);

CREATE TABLE users (
  id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,            -- argon2id
  must_change_password INTEGER NOT NULL DEFAULT 0,
  disabled_at INTEGER, created_at INTEGER NOT NULL);

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,                    -- sha256 of cookie value
  user_id INTEGER NOT NULL REFERENCES users ON DELETE CASCADE,
  csrf TEXT NOT NULL, expires_at INTEGER NOT NULL);

CREATE TABLE clients (
  id TEXT PRIMARY KEY, name TEXT NOT NULL,
  token_hash TEXT UNIQUE NOT NULL,        -- sha256; tokens are 256-bit random
  created_by TEXT NOT NULL,               -- 'user:<id>' | 'client:<id>'
  created_at INTEGER NOT NULL, last_used_at INTEGER, revoked_at INTEGER);

CREATE TABLE workers (
  id TEXT PRIMARY KEY, token_hash TEXT UNIQUE NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('pending','active','paused','rejected')),
  hostname TEXT, description TEXT,
  cpu_model TEXT, cores INTEGER, mem_bytes INTEGER, os TEXT, arch TEXT,
  engine_version TEXT, ffmpeg_version TEXT,
  sandbox_level TEXT,                     -- 'full' | 'fs-only' | 'none' (§7.4)
  slots INTEGER, models TEXT,             -- JSON
  registered_at INTEGER NOT NULL, decided_by TEXT, decided_at INTEGER,
  last_seen_at INTEGER, gauges TEXT);     -- JSON, latest heartbeat-reported gauges

CREATE TABLE grants (
  principal TEXT NOT NULL,                -- 'user:<id>' | 'client:<id>' | 'worker:<id>'
  role TEXT NOT NULL,
  scope TEXT NOT NULL,                    -- '*' | 'queue:<id>' | 'worker:<id>'
  granted_by TEXT NOT NULL, granted_at INTEGER NOT NULL,
  PRIMARY KEY (principal, role, scope));

CREATE TABLE jobs (
  id TEXT PRIMARY KEY, queue_id INTEGER NOT NULL REFERENCES queues,
  client_id TEXT NOT NULL, idempotency_key TEXT,
  state TEXT NOT NULL,
  weight TEXT NOT NULL DEFAULT 'medium' CHECK (weight IN ('low','medium','high')),
  model TEXT NOT NULL, language TEXT NOT NULL, prompt TEXT, label TEXT, source TEXT,
  metadata TEXT,                          -- opaque client JSON, ≤ 8 KiB
  original_bytes INTEGER, original_sha256 TEXT, original_type TEXT,  -- sniffed
  original_purged_at INTEGER,
  canonical_bytes INTEGER, canonical_sha256 TEXT, audio_samples INTEGER,
  canonical_purged_at INTEGER,
  created_at INTEGER NOT NULL, started_at INTEGER, completed_at INTEGER,
  compute_ms INTEGER, error TEXT,
  first_downloaded_at INTEGER, last_downloaded_at INTEGER,
  download_count INTEGER NOT NULL DEFAULT 0,
  UNIQUE (client_id, idempotency_key));
CREATE INDEX jobs_queue_state ON jobs (queue_id, state, created_at);

CREATE TABLE tasks (
  job_id TEXT NOT NULL REFERENCES jobs ON DELETE CASCADE, seq INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('prepare','transcribe')),
  start_sample INTEGER, end_sample INTEGER,   -- NULL for prepare
  cost REAL NOT NULL,                         -- scheduling cost (§8.2)
  state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  result TEXT,                                -- JSON segments (transcribe), times relative to task start
  engine_version TEXT, worker_id TEXT, compute_ms INTEGER, error TEXT,
  PRIMARY KEY (job_id, seq));
CREATE INDEX tasks_pending ON tasks (job_id, seq) WHERE state = 'pending';

CREATE TABLE leases (
  id TEXT PRIMARY KEY, job_id TEXT NOT NULL, seq INTEGER NOT NULL,
  worker_id TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
  ended_at INTEGER, outcome TEXT,             -- done | expired | released | failed | cancelled
  FOREIGN KEY (job_id, seq) REFERENCES tasks ON DELETE CASCADE);
CREATE INDEX leases_live ON leases (expires_at) WHERE ended_at IS NULL;

CREATE TABLE sched_vtime (                    -- §8.2
  queue_id INTEGER NOT NULL REFERENCES queues ON DELETE CASCADE,
  class TEXT NOT NULL,                        -- '' = queue level; 'low'|'medium'|'high'
  vtime REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (queue_id, class));

CREATE TABLE audit (
  ts INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL,
  target TEXT, detail TEXT);
```

Notes:
- Timestamps are Unix milliseconds in UTC.
- Migrations are embedded (`embed.FS`), applied on start, and versioned with `PRAGMA user_version`.
- Invariant: at most one live lease per task (`ended_at IS NULL`). The claim transaction enforces it, and a test pins it (§11).

---

## 5. Authorization

### 5.1 Principals and authentication

| Principal | Credential | Transport |
|---|---|---|
| User | username + password, then a session cookie (`HttpOnly`, `Secure`, `SameSite=Lax`) plus a CSRF token on mutating forms | `/ui` |
| Client | `Authorization: Bearer wnc_<base62>` | API |
| Worker | `Authorization: Bearer wnw_<base62>` | API |

- The prefixes let secret scanners recognize leaked tokens. Tokens are 256-bit random values stored as sha256. That is enough for high-entropy secrets; argon2 is only needed for passwords.
- A token is shown exactly once, at creation.
- A user may also call the API with a session, so the UI and API share handlers. While the user's `must_change_password` is set (an administrator chose or generated the password), every API route refuses the session with `403` `password-change-required`; the UI handles the change itself. Creating a session re-checks, in the same transaction, that the password hash the login verified is still current and the user is not disabled, so a reset or disable that lands mid-login wins. A self-service password change does the same: the new password is set only if the hash the current password was checked against is still current and the user is not disabled, so an administrator's reset or disable that lands between the check and the change is not overwritten.

### 5.2 Roles

| Role | Scope | Allows |
|---|---|---|
| `admin` | `*` | Everything, including creating users and queues, **setting queue weights**, and granting roles. |
| `queue_admin` | `queue:X` | Approve or reject workers **for queue X** (i.e. grant `process` on X), grant and revoke `submit`, `view_all`, `delete_all` and `process` on X, see all jobs on X. Cannot grant or revoke `queue_admin` (queue admins cannot make or remove other queue admins). Cannot change X's weight, because weight is a fleet-wide resource decision. |
| `worker_admin` | `*` or `worker:W` | Approve, pause, resume and reject the worker(s) in scope. With scope `*`, may also grant `worker_admin` on `worker:<id>`. Cannot grant queue access unless also `queue_admin` for that queue. |
| `submit` (client) | `queue:X` | Create jobs on X. See and delete its own jobs on X. |
| `view_all` (client) | `queue:X` | See, and download transcripts of, all jobs on X. |
| `delete_all` (client) | `queue:X` | Delete any job on X. *Open item; see §13.* |
| `client_admin` | `*` | Create and revoke clients via `POST /client`. When creating a client it may hand on grants it holds itself (no escalation); this re-delegation applies only at creation. Revoking a client requires authority over every grant that client holds. |
| `process` (worker) | `queue:X` | Lease tasks (prepare and transcribe) from X. |

**Approving a worker is two separate acts.** Lifecycle is `worker_admin`. Data access is `queue_admin`. Data access is the trust decision, because a worker receives that queue's media. The UI shows both on one approval screen, together with the worker's `sandbox_level`. Clicking "Approve for queues A, B" works only if the actor holds both rights for every queue chosen.

A worker in `active` state with no `process` grants is legal and simply idle.

**Grant management.** Granting to, or revoking from, an existing principal requires `admin`, or `queue_admin` on queue X for `submit`, `view_all`, `delete_all` and `process` on `queue:X`, or (for `worker_admin` on `worker:<id>` only) `worker_admin` on `*`. Revoking needs exactly the authority that granting would. Revoking a worker's `process` grant also cancels, in the same transaction, the worker's live leases on every queue it no longer holds `process` on (outcome `cancelled`; the task requeues without using an attempt, as on rejection), so withdrawn access to a queue's media takes effect at once. Holding a role never by itself lets you hand it on: re-delegating your own grants is allowed only when creating a client (`client_admin`, above). Revoking a client's token requires `client_admin` plus authority over every grant the client holds, so a client admin cannot revoke a client more privileged than itself. Grant listings do not reveal whether a principal exists.

#### 5.2.1 Audit

Every change to authorization or to stored state that a person, client or worker causes is recorded in the audit log (`GET /admin/audit`, filterable by action prefix, actor, target and time; the `/ui/audit` page). **A change and its audit entry commit in the same database transaction:** if the audit entry cannot be written, the change fails and nothing is applied. Changes the server makes itself (retention, pruning, session expiry, media purges) use the actor `system`; the offline admin command uses `admin-cli`.

| Action | Recorded when | Actor |
|---|---|---|
| `grant.add` / `grant.remove` | A grant is given or taken away | the granting principal |
| `client.create` / `client.revoke` | An API client is created or its token revoked | the acting principal |
| `user.create` / `user.disable` / `user.reset_password` | An administrator manages a UI user | the acting principal |
| `user.change_password` | A user changes their own password (all their sessions end) | the user |
| `auth.login` / `auth.logout` | A UI session starts (including the rotation after a password change) or ends | the user |
| `auth.login_failed` | A sign-in is refused. The target names the user only if the username exists, so a password typed into the username field is never stored | `anonymous` |
| `session.expire` | The reaper deletes expired sessions (one entry with the count) | `system` |
| `queue.create` / `queue.patch` / `queue.rename` | A queue is created or changed | the acting principal |
| `worker.register` | A worker registers (`POST /worker`) | the worker |
| `worker.update_facts` | A worker replaces its reported facts | the worker |
| `worker.approve` / `worker.pause` / `worker.resume` / `worker.reject` | Worker lifecycle and queue access decisions | the acting principal |
| `worker.prune` | The reaper deletes a pending worker past `--pending-worker-ttl` or a rejected worker past `--rejected-worker-retention` (one entry per worker) | `system` |
| `job.create` | A job is created by upload (a replayed idempotency key is not a new entry) | the submitting principal |
| `job.delete` | A job is deleted by its owner, by a principal with `delete_all`/`admin`, or by retention | the deleting principal, or `system` |
| `job.purge_media` | An original upload or canonical audio is deleted (§8.5) | `system` |

Audit details never contain secrets or requester content: no passwords, tokens, prompts or metadata.

**Not audited: the task and lease lifecycle.** Claims, heartbeats, result uploads, task completion and failure, lease expiry and release, and transcript download counts are recorded in the structured logs (§10.1: `task.leased`, `task.done`, `task.failed`, `lease.expired`, …) and the metrics (§10.2), not in the audit table. This is deliberate. They are high-volume machine events, one per task per attempt and one per heartbeat, that would bury the audit log and grow it without bound. The audit table is for decisions: changes made by people, clients or workers to authorization and stored state, security-relevant events (sign-ins, refused sign-ins), and the system's own retention and pruning actions. What those machine events can affect is bounded by audited decisions (who may submit, which worker may process which queue), and lease cancellations caused by an audited change are counted in that change's audit entry (`grant.remove`, `worker.reject`).

### 5.3 Visibility rules

- `GET /queue/{q}` lists jobs where `client_id = me`, or every job if the caller holds `view_all`/`queue_admin`/`admin` on q.
- `GET /job/{id}` returns **404, not 403**, when the caller can't see the job, so job IDs can't be probed.
- Workers can't see jobs at all. They see only their own leases.

---

## 6. HTTP API

All bodies are JSON unless noted. Errors use RFC 9457 `application/problem+json` with a stable `type` slug (`lease-expired`, `worker-paused`, `media-invalid`, …). Paths are unversioned. A breaking change would add `/v2` beside them; that is YAGNI until it happens.

### 6.1 Client API

| Method & path | Purpose | Auth |
|---|---|---|
| `POST /queue/{q}` | Create a job. Multipart body: part `params` (JSON) and part `media` (any audio or video). Optional `Idempotency-Key` header. Returns `201` with `Location: /job/{id}` and the job JSON. | `submit` on q |
| `GET /queue/{q}` | List visible jobs. Query: `state`, `since` (created or updated after), `limit`, `cursor`. | `submit` or `view_all` on q |
| `PATCH /queue/{q}` | Change `weight`, `description` or `max_upload_bytes`. | `admin` (weight); `queue_admin` (the others) |
| `GET /job/{id}` | Job status (below). | owner, or `view_all` on its queue |
| `GET /job/{id}/transcript` | `text/plain` transcript with the provenance header. Records a download. `409` if not done. | same |
| `GET /job/{id}/result` | JSON segments with absolute times, for consumers that want structure. Also records a download. | same |
| `DELETE /job/{id}` | Delete the job, its blobs and live leases. `204`. | owner, `delete_all`, or `queue_admin` |
| `POST /client` | Create a client with a set of grants (a subset of the caller's). Returns the token once. | `client_admin` |
| `GET /client/{id}`, `DELETE /client/{id}` | Inspect or revoke. | `client_admin` or self (GET) |

**Job create params**

```json
{
  "model": "small.en",               // required; must be on the server's model allowlist
  "language": "en",                  // default "en"; "auto" allowed
  "weight": "medium",                // optional: low | medium | high (default medium)
  "prompt": "This is a project planning meeting. Alice and Bob discuss the budget. …",   // ≤ 800 chars
  "label": "meeting-2026-01-15",
  "source": "https://youtube.com/watch?v=…",   // recorded, never fetched
  "metadata": { "requester": "…", "session": "…" }   // opaque, ≤ 8 KiB
}
```

Model choice stays with the client. The rule "`en` → `small.en`, anything else → `large-v3-turbo`" is client policy and lives in the client.

**Job status**

```json
{
  "id": "0193…", "queue": "public", "state": "running", "weight": "medium",
  "model": "small.en", "language": "en", "label": "…", "source": "…", "metadata": {…},
  "media": { "original_bytes": 1020000000, "original_type": "video/mp4",
             "duration_s": 12890.4, "original_purged": true, "audio_purged": false },
  "tasks": { "total": 5, "pending": 1, "leased": 2, "done": 2, "failed": 0 },
  "created_at": "…", "started_at": "…", "completed_at": null,
  "wait_s": 41.2, "elapsed_s": 1830.0, "compute_s": 2950.7,
  "downloads": { "count": 0, "first": null, "last": null },
  "error": null
}
```

"How long it took" is reported three ways: `wait_s` (created → first lease), `elapsed_s` (first lease → completed), and `compute_s` (sum of task compute time, including prepare). With parallel tasks, compute can exceed elapsed.

**Upload handling (server, streaming)**
- Stream to `blobs/{job}/original.part` while hashing and enforcing `max_upload_bytes` (default 2 GiB, since video is now accepted). Fsync, rename, then commit the DB row. A crash leaves at most an orphan `.part`, which the reaper deletes.
- **Admission.** A client (or user) may have at most `--max-uploads-per-principal` uploads in progress (default 2); more get `429` with `Retry-After`. Free disk is reserved as the media bytes arrive, at most 1 MiB ahead of the writer and never from the declared `Content-Length`, so a slow upload holds no more than what it has sent plus one step; near the floor the writer reserves just the bytes in hand rather than a whole step. A `Content-Length` larger than free space above `--min-free-bytes` is refused with `507` before the body is read; an upload that would take free space below `--min-free-bytes` stops with `507`. The body must average at least `--min-transfer-rate` over every sliding window of max(`--read-timeout`, 30 s), so a trickle cannot hold an upload slot and its reservation for hours; a slower body is cut off with `408`.
- **Sniff the type, don't trust it.** Read the leading bytes with Go's `http.DetectContentType` plus checks for RIFF/WAVE and `fLaC`. The client's Content-Type is ignored.
- **Fast path:** if the file is a valid canonical stream, it becomes the canonical audio directly and no prepare task is created. Canonical means PCM WAV or FLAC, 16 kHz, mono, 16-bit, fully decoded by the pure-Go validator.
- **Otherwise:** create a `prepare` task. The server does not judge whether ffmpeg can decode the file; the prepare task fails with ffmpeg's reason if it can't.
- `Idempotency-Key`: the same (client, key) returns the original job with `200` instead of creating a duplicate. This matters because large WAN uploads get retried.

### 6.2 Worker API

| Method & path | Purpose | Auth |
|---|---|---|
| `POST /worker` | Register. Body: facts (below). Returns `201` `{id, token, state:"pending"}`. Rate-limited per source IP; at most `--max-pending-per-source` workers from one address (IPv6: one /64) may await approval at once (`429`), and `--max-pending-workers` in total (`503`). | none |
| `GET /worker/{id}` | Own state and grants; config hints (heartbeat interval, poll interval). | that worker, or an admin |
| `PATCH /worker/{id}` | Update facts (engine versions, models, slots, sandbox level) after restart or upgrade. | that worker |
| `POST /worker/{id}/leases?wait=30s` | Claim one task. Body: `{"kinds":["prepare","transcribe"],"models":["small.en"]}`, describing what this slot can run **right now** given free memory. Returns `201` with the lease, or `204` on timeout. | active worker |
| `GET /lease/{id}/input` | Prepare: the original bytes. Transcribe: a WAV stream (16 kHz mono s16) decoded from the canonical FLAC for this task's sample range. `Range` is supported for prepare; transcribe input is regenerated on retry. | lease holder, live lease |
| `POST /lease/{id}/heartbeat` | Extend the lease. Optional body: worker gauges (§10.2). Returns `200 {expires_at}`, or `410` if expired, cancelled or the job was deleted, or if the lease may not be extended any more: the worker is neither active nor paused, no longer holds `process` on the lease's queue, or the lease is past its maximum age (§8.3). The worker must then kill the child. | lease holder |
| `PUT /lease/{id}/result` | Prepare: body is the canonical FLAC (`audio/flac`). Transcribe: segments JSON. Idempotent for the same lease. `410` if the lease is no longer live; `422` if validation fails (counts as a failed attempt). | lease holder |
| `POST /lease/{id}/failure` | `{"reason":"…","stderr_tail":"…","retryable":true}`. Ends the lease; the task requeues if attempts remain and `retryable`. | lease holder |
| `DELETE /lease/{id}` | Voluntary release on graceful shutdown. Does not consume an attempt. | lease holder |
| `GET /models/{name}` | Model file (ggml), sha256 in `ETag`. The server keeps a cache, filled from upstream on first request and pinned by hash. | any active worker |

**Registration facts**

```json
{ "hostname": "nuc-01", "description": "garage NUC, nights only",
  "cpu_model": "Example CPU", "cores": 8, "mem_bytes": 8589934592,
  "os": "linux", "arch": "amd64",
  "engine_version": "whisper.cpp 1.9.5", "ffmpeg_version": "7.1.1",
  "sandbox_level": "full", "kinds": ["prepare","transcribe"],
  "slots": 1, "models": ["small.en", "large-v3-turbo"] }
```

The description defaults to `$WHISPER_DESCRIPTION` or the hostname.

**Lease** (claim response)

```json
{ "id": "lse_…", "kind": "transcribe", "job": "0193…", "seq": 2,
  "input_url": "/lease/lse_…/input", "audio_samples": 57600000,
  "model": "small.en", "model_sha256": "…", "language": "en", "prompt": "…",
  "heartbeat_interval_s": 30, "expires_at": "…" }
```

**Transcribe result**

```json
{ "engine_version": "whisper.cpp 1.9.5", "model": "small.en", "model_sha256": "…",
  "vad": "silero-v5.1.2", "language": "en", "compute_ms": 1180000,
  "segments": [ { "start_ms": 0, "end_ms": 4120, "text": "…" } ] }
```

The server validates the result: segment times must be monotonic, within the task's duration, with bounded text size. Times are converted to absolute using the task's start sample.

### 6.3 UI (`/ui`)

`/` redirects to `/ui`. Pages:

- **Dashboard**: queue depths, running tasks, workers online, throughput (audio-hours per hour), and each queue's **actual vs configured share** (§8.2).
- **Workers**: pending first, with the facts list (including sandbox level) and approve (queue picker), pause, resume and reject buttons; last seen; current leases.
- **Queues**: create, edit (weight is admin-only) and list jobs; per-queue grants.
- **Jobs**: filters, detail view, delete, transcript download.
- **Users and clients**: create, disable, grant roles, mint tokens (shown once).
- **Audit log**: every approval, grant, weight change, deletion and token mint.

---

## 7. Worker design

### 7.1 Lifecycle

1. Load `state/credentials.json` from the state dir (default `/var/lib/whisper-network`). If it is missing, `POST /worker` and save it with mode 0600.
2. Probe facts: cores (cgroup quota aware), memory (cgroup limit aware), `whisper-cli --version`, `ffmpeg -version`, available models, and the sandbox level (§7.4).
3. While `pending`, poll `GET /worker/{id}` with backoff (30 s → 5 min). Log the ID and description once, so the operator knows what to approve.
4. Once `active`, start N slot goroutines (§7.3).
5. On SIGTERM: stop claiming, then by default `DELETE` every live lease and kill the children, so tasks requeue immediately instead of waiting out the lease timeout. With `--drain`, finish running tasks first.

**Without a state volume**, every container restart registers a new worker and the pending list fills with dead entries. Mitigations: the docs say to mount the volume, and the reaper deletes `pending` workers unseen for 7 days. Rejected workers stay for the audit trail.

### 7.2 Slot loop

```
loop:
  kinds, models := whatFitsInFreeMemory()
  lease := POST /worker/{id}/leases?wait=30s {kinds, models}
  if 204: continue
  start heartbeat goroutine (every heartbeat_interval; on 410 → cancel ctx)
  switch lease.kind:
    prepare:    download input (Range resume) → sandbox-exec ffmpeg → PUT result (FLAC)
    transcribe: ensure model (download, verify sha256) → download WAV → sandbox-exec whisper-cli → PUT result (JSON)
  on child error: POST failure with stderr tail
  cleanup tmp
```

- No local queueing. A slot claims only when it is free, and the download happens inside the lease, covered by heartbeats. Prefetching would be local queueing and is deliberately left out.
- Heartbeats run independently of the child process, so a long whisper run never starves them.
- **Prepare command:** `ffmpeg -nostdin -hide_banner -loglevel error -i in -map 0:a:0 -vn -sn -dn -ac 1 -ar 16000 -sample_fmt s16 -c:a flac -compression_level 5 out.flac`. The first audio stream is picked; selecting among multiple streams is a later concern.
- **Transcribe command:** `whisper-cli -m model -f in.wav -l lang -t threads --prompt … -oj -of out`. The JSON output is converted to the result schema.

### 7.3 Concurrency: is "one job per CPU" right?

**No.** whisper.cpp is multithreaded per job, and the encoder is GEMM-bound. Running one job per core with one thread each:
- multiplies memory by the core count (each process loads its own model and KV cache), and
- makes each job ~4–8× slower, so a 3-hour meeting takes most of a day.

Throughput per core does fall off above roughly 4–8 threads per process, because memory bandwidth saturates. So several mid-width jobs beat one job that spans every core.

**Rule:**

```
threads_per_slot = config (default 4)
slots_by_cpu     = max(1, floor(usable_cores / threads_per_slot))
slots_by_mem     = floor((mem_limit − reserve) / peak_rss(largest model offered))
slots            = min(slots_by_cpu, slots_by_mem)
```

- `usable_cores` and `mem_limit` come from the cgroup limits when present, so the rule respects what the container was actually given.
- Per-claim memory accounting means a slot only offers a model whose peak RSS fits in the memory not already reserved by other slots. A big-memory model never lands on a box that can't hold it.
- Prepare tasks are ffmpeg-bound and short (minutes of CPU per hour of video, reasoned). They use a slot like any task, with a small fixed memory reservation.
- `peak_rss` is a per-model table shipped with the binary and refined by `bench`.

**Measurement (v1):** `whisper-network bench --model small.en --threads 2,4,8` runs a bundled 60 s clip and reports realtime factor and peak RSS. Its output tunes the defaults. The numbers above are reasoned, not measured. Confidence: medium.

### 7.4 Priority and sandbox

Every child runs under `whisper-network sandbox-exec -- <cmd>`. The worker re-execs itself into this wrapper, which applies the restrictions below to itself and then calls `execve` on the target. Landlock domains, the seccomp filter, rlimits, `no_new_privs` and the dropped capabilities survive `execve`, so they apply to the child without affecting the worker.

| Control | Mechanism | Applies to |
|---|---|---|
| Lowest CPU priority | `nice 19`, `SCHED_IDLE` (`sched_setattr`) | both |
| Lowest I/O priority | `ioprio_set` class idle | both |
| Filesystem | Landlock, newest rights the kernel has (go-landlock V11, best effort): read the input, the model, the system library paths, the tool binary and its directory if dedicated; write only the task's directory. Device ioctls (ABI ≥ 5) and connecting to pathname Unix sockets (ABI ≥ 9) are denied too | both |
| Network | seccomp filter: `socket(2)` and `socketpair(2)` fail with `EACCES` for every family (TCP, MPTCP, UDP, raw, netlink, Unix pathname and abstract); `io_uring` is disabled. `socketpair` is refused because an `AF_UNIX` datagram socket from it can `sendto` any pathname socket (`/dev/log`, the journald socket) that Landlock below ABI 9 does not check. The child inherits only `/dev/null` and pipes, never a socket, so `sendto`/`sendmsg` with an address has nothing to send from. Landlock adds TCP (ABI ≥ 4) and UDP (ABI ≥ 10) denial as a second layer | both |
| IPC and signals | Landlock scoping (ABI ≥ 6): no abstract Unix sockets or signals to processes outside the sandbox. Below ABI 6 the seccomp filter allows signals only to the child itself and its process group | both |
| Process tree | The child leads its own process group; seccomp denies `setsid`/`setpgid`, so descendants cannot leave it. Cancel, timeout and normal exit all `SIGKILL` the whole group; `PR_SET_PDEATHSIG` kills the child if the worker dies | both |
| Namespaces | seccomp denies `unshare` and `clone` with any `CLONE_NEW*` flag, and `setns`, with `EPERM`; `clone3`, whose flags a filter cannot read, fails with `ENOSYS`, so glibc (2.34+) falls back to `clone` for threads and `posix_spawn`. The Go runtime uses `clone` directly. No user namespace means no namespace-local capabilities to attack the kernel with | both |
| Privilege | `no_new_privs`; capability bounding, inheritable and ambient sets emptied; a root worker must name a separate `--child-uid`/`--child-gid` (it refuses to start otherwise), which cannot read `credentials.json`. The child uid must be dedicated to the worker's children: the worker refuses to start while `/proc` shows any task of that uid | both |
| Environment | Allow-list: `PATH`, `LANG`, `LC_*`, `TZ`, `GGML_*`, `OMP_*`, `CUDA_*`; `HOME` and `TMPDIR` are the task directory | both |
| Resources | `RLIMIT_AS`, `RLIMIT_FSIZE`, `RLIMIT_NOFILE`, `RLIMIT_NPROC` (below), wall-clock timeout scaled to the input's duration | both |
| Files | The worker creates the input and whisper-cli's response file in the child-owned task directory with `O_CREAT|O_EXCL|O_NOFOLLOW`, mode 0600, and `fchown`s them to the child uid. It opens `out.flac`/`out.json` with `O_NOFOLLOW|O_NONBLOCK` and requires a regular file with one link, within a size bound (JSON ≤ 64 MiB, FLAC ≤ `RLIMIT_FSIZE`). Inputs are downloaded only up to the size the lease states (`input_bytes` for prepare, `audio_samples` for transcribe); a lease without it fails, not retryable | — |

- **Checked 2026-11-15:** the default seccomp profiles of Docker (`moby/profiles`) and Podman (`containers/common`) both allow the `landlock_*` syscalls and stacking a seccomp filter after `no_new_privs`. That makes Landlock plus seccomp the unprivileged, in-container mechanism available. `unshare -n` is not an option, because it needs `CAP_SYS_ADMIN` under Docker's default profile.
- **`RLIMIT_NPROC`.** The kernel checks it at `fork` against every task (process or thread) of the child's real uid, in all PID namespaces, and all slots' children share that uid. So the worker gives them one shared allowance, `256 × slots`, rather than a per-child snapshot that another slot's child may already have used up. With `--child-uid` (dedicated, checked at startup) it is the absolute limit (`sandbox-exec --nproc-limit`): together, all children never exceed it. Without a child uid, children run as the worker's own uid, whose other tasks the worker does not own, so `sandbox-exec --nproc` sets the uid's task count at launch, as `/proc` shows it, plus the allowance.
- **The tool's directory** is granted whole only when it is outside the system paths (whisper.cpp's shared libraries and ggml's CPU backends sit next to `whisper-cli`, e.g. `/opt/whisper`). The worker refuses to start if such a directory is under `$HOME` or contains the state directory, or if the state directory is under any read-only path.
- **Residual gaps:**
  - Children of one worker share a uid (the worker's, or `--child-uid`). Each runs in its own Landlock domain, which denies `ptrace` across domains at every ABI and, from ABI 6, signals; but per-uid kernel limits are shared, so one child can exhaust the `RLIMIT_NPROC` allowance of the others (bounded by it, never beyond). The kernel counts across PID namespaces, while the worker sees only its own `/proc`, so another container running as the same uid would go unnoticed by the startup check and narrow the allowance: the child uid must be dedicated.
  - A non-root worker's children run as the worker's own uid. Landlock keeps them away from `credentials.json` by path; a separate uid adds a second layer and needs a root worker with `--child-uid`.
  - The process-group kill relies on seccomp. Where the filter is unavailable (an architecture other than amd64/arm64, or a kernel without seccomp), a descendant could `setsid` away, and the self-test reports the open socket checks.
  - Below Landlock ABI 6, a descendant of the child cannot signal its own pid (`raise` in a grandchild fails); ffmpeg and whisper-cli do not fork.
- **Self-test.** At startup the worker runs a child under a prepare task's spec that tries each escape: read a forbidden file, TCP connect, MPTCP connect, UDP `sendto`, connect to a pathname Unix socket outside the allowed paths, connect to an abstract Unix socket, `socketpair(AF_UNIX, SOCK_DGRAM)` then `sendto` a pathname datagram socket outside the allowed paths (`unix_dgram_sendto`), and `kill(parent, 0)`. The level is `full` only if every check is refused (a protocol the kernel lacks counts as refused), `fs-only` if the file read is refused but some network/IPC check is not, else `none`. The worker logs the Landlock ABI and which checks held.
- **`--prepare-min-sandbox`** (server, default `full`) controls which workers get prepare tasks. Prepare tasks go only to workers that meet this level. Transcribe tasks, whose input is server-generated WAV, go to any level.
- The operator should still give the container a low `--cpu-shares` and a memory limit. This is documented, since the worker cannot set it on itself.

Confidence that this holds across the fleet's kernels: medium-high for kernels with Landlock ABI ≥ 6 on amd64/arm64, where the self-test has been observed to refuse every check; lower elsewhere, which the self-test reports.

### 7.5 Platforms

- Container images: `linux/amd64` and `linux/arm64`, with whisper.cpp built for the CPU baseline. AVX2/NEON dispatch is done by ggml at runtime.
- **macOS:** Docker on Apple Silicon has no Metal, which costs a large factor. The worker therefore also ships as a native darwin/arm64 binary and finds `whisper-cli`/`ffmpeg` on `$PATH` (Homebrew). There is no Landlock on macOS, so `sandbox_level: none` and macOS workers get no prepare tasks under the default policy. That suits them, since they are best spent on transcription.
- **Windows:** the worker is not supported and refuses to start: there is no confinement for its child processes (issue #26). The server and client CLI build for Windows.
- **GPU** workers are out of scope for v1. The slot model generalizes to them (one slot per GPU, a different `peak_rss` budget), so nothing in the API blocks it.

---

## 8. Server behaviour

### 8.1 Media pipeline

```
upload ─▶ sniff ─┬─ canonical WAV/FLAC (Go-validated) ─────────────────┐
                 └─ anything else ─▶ prepare task (worker, sandboxed)  │
                                        │ PUT FLAC                     │
                                        ▼                              ▼
                               Go-validate FLAC ─▶ store canonical.flac ─▶ purge original
                                                         │
                                         energy scan ─▶ split points ─▶ transcribe tasks
                                                         │
                         GET /lease/{id}/input ◀── decode sample range → WAV stream
```

- **Canonical storage is always 16 kHz mono 16-bit FLAC.** A fast-path WAV upload is encoded to FLAC by the server in pure Go (§8.1.1); a fast-path FLAC upload is decoded fully to validate it and stored as-is. Everything downstream (splitting, serving) deals with one format.
- **Validation of prepare output is a full decode in pure Go** (`mewkiz/flac`), checking sample rate, channels, bit depth, total samples (≤ 12 h) and that the stream decodes without error. Go is memory-safe, so a malicious FLAC from a compromised worker can at worst fail validation or waste CPU, and the CPU is bounded by the size cap.
- **Analysis CPU is admission-controlled.** Validation, analysis and encoding hold one of GOMAXPROCS analysis slots, and one principal may hold at most `--max-analyses-per-principal` of them (default 1); further analyses wait up to 30 s, then get `429`. The decode loops check for cancellation before every frame, so a client that disconnects stops its analysis within one frame. CPU per input byte is bounded too: a fast-path FLAC averaging more than 32 decoded samples per frame byte (metadata blocks such as PADDING are not counted; measured: constant frames of digital silence reach ~340, real speech ~0.8, a noise floor 2–4, 11 s of speech followed by 10 min of digital silence ~38) is not decoded on the server but sent to a prepare task, so a sandboxed worker spends that CPU.
- **Split points:** `target = 60 min` (configurable), `window = ±60 s`. Inside each window, compute RMS energy over 100 ms frames and cut at the quietest frame. There is no overlap, so no de-duplication is needed when stitching. Jobs shorter than `target + window` get a single task. Words cut at a seam can be mangled; fixed-length chunking has the same problem, and cutting at silence makes it rarer.
- **Serving transcribe input as WAV** decoded on the fly (FLAC supports seek): whisper-cli then only ever parses WAV, and the server keeps the compressed copy. Bandwidth is not the binding constraint. WAV is ~115 MB per audio-hour, and an audio-hour takes ≥ 20 min of compute at `small.en`, so it's ~0.1 MB/s per slot.
- **Retention:** the original is purged as soon as canonical audio is accepted. The canonical audio follows `audio_retention` (§8.5).

#### 8.1.1 FLAC: what was checked (2026-11-15)

| Question | Finding | Source |
|---|---|---|
| Can `whisper-cli` 1.9.5 read FLAC? | **Yes.** `examples/common-whisper.cpp` loads all input through miniaudio 0.11 with WAV, MP3 and FLAC decoders compiled in (only device I/O, encoding, etc. are disabled). | whisper.cpp tag `v1.9.5` |
| Does it resample? | **Yes.** The decoder is configured for f32, mono, `WHISPER_SAMPLE_RATE` (16 kHz), so miniaudio resamples any rate. | same |
| Other formats? | Only when built with `WHISPER_FFMPEG`, which links libav **into whisper-cli**. Rejected: that would put the ffmpeg attack surface inside the transcribe child, with no separate prepare step. | `CMakeLists.txt` |
| Can Go **decode** FLAC? | Yes. `mewkiz/flac` v1.0.9 (pure Go) supports decode and sample seek (`NewSeek`, `Seek`). | `mewkiz/flac` |
| Can Go **encode** compressed FLAC? | **Yes, as of v1.0.14** (the pinned version). The encoder runs fixed-predictor analysis (orders 0–4, Rice partition order 0) by default. LPC selection is not automatic, so output is somewhat larger than ffmpeg's (not yet measured). *An earlier revision of this spec said "no"; that was based on v1.0.9.* | `analysis_fixed.go`, `encode_frame.go` |

What follows from this:
- Arbitrary media still needs ffmpeg, which is why prepare runs on workers. The server only decodes and validates FLAC, and encodes FLAC from already-validated PCM WAV. Both are pure Go, so the server never runs a native media parser.
- whisper-cli could take FLAC or MP3 directly, but the design does not rely on that. Feeding it server-generated WAV keeps the child's input surface to a trivial format and makes resampling deterministic, done once by ffmpeg.
- Storage saving: speech FLAC at 16 kHz mono is typically 40–60 % of WAV (reasoned, not measured on our corpus). For a hypothetical 500 h backlog that's ~25–35 GB instead of ~58 GB, transient.

### 8.2 Scheduling: weighted fair queueing

**Inputs**
- **Queue weight** `W_q`: an integer 1–10000, default 100, set only by `admin`.
- **Job weight class** `c ∈ {low, medium, high}`, default `medium`. Class weights `w_c` default to 1 / 2 / 4 (server config).
- **Task cost** = audio seconds × `model_factor` (`small.en` = 1, `large-v3-turbo` = 3; server config, and calibration comes from the realtime-factor metric). A prepare task costs a fixed 60. Cost is computed when the task is created and stored on it.

**Algorithm: two-level start-time fair queueing over virtual time**

Each queue has a virtual time `V_q`, and each (queue, class) pair has `V_qc`. On every claim:

1. **Candidates** are (q, c) pairs that have a pending task this worker may take: q is granted to the worker, the kind is offered, the model is offered, and for prepare the sandbox level is met.
2. Pick q with the minimum `V_q`. Within q, pick c with the minimum `V_qc`. Within (q, c), pick the oldest job and its lowest pending seq. Ties break by queue ID.
3. Charge the cost: `V_q += cost / W_q` and `V_qc += cost / w_c`.
4. **Reactivation rule:** when a queue goes from having no pending work to having some (job created, or prepare finished), set `V_q = max(V_q, V_min)`, where `V_min` is the minimum `V` over queues that currently have pending work, or the last charged `V` if none do. Classes within a queue follow the same rule. This stops an idle queue from banking credit and then monopolising the fleet when it wakes up.

**Properties**
- While queues A and B are both backlogged and served by the same workers, the cost each receives converges to `W_A : W_B`. The deviation at any moment is bounded by one task's cost. For example, `public=100` and `meetings=300` gives meetings 75 % of the fleet while both have work, and lets either use 100 % alone.
- Within a queue, backlogged classes share 4 : 2 : 1. `low` is never starved, only slowed. This is weighting, not strict priority, as requested.
- Inside a class, ordering is FIFO by job. A huge job ahead of a small one in the same class and queue delays it. Raising the small job to `high` is the remedy.
- **Eligibility caveat:** when workers are granted different queue subsets, each claim is fair among *that worker's* candidates. The fleet-wide shares then approximate the weights rather than hit them exactly; this is the usual limitation of greedy fair queueing with heterogeneous servers. The dashboard and the metrics show actual against configured share, so this is visible rather than hidden.
- Cost is charged at lease time, and an expired or failed lease is not refunded. The re-run is charged again. Refunding would add bookkeeping to fix a rare, small error. See §13.
- Weight changes apply from the next charge; no retroactive correction.

**Implementation**
- All of this runs inside the claim's `BEGIN IMMEDIATE` transaction:
  1. One grouped query gets the candidate (q, c) pairs and their oldest pending task: `SELECT j.queue_id, j.weight, MIN(j.created_at) … WHERE t.state='pending' AND <eligibility> GROUP BY 1, 2`.
  2. Load the `sched_vtime` rows for those pairs, pick in Go (`sched` package, pure), then update the task, the lease and the vtimes.
- With tens of queues this is microseconds. SQLite's single writer guarantees that no two claims see the same snapshot.
- Long-poll: if there are no candidates, the handler waits on the dispatcher's notification channel or the `wait` deadline, then retries once. Notifications are hints; the transaction is authoritative.

Confidence: high that this gives the fairness asked for. Medium on the default parameters (class weights, model factors), which should be tuned from the `whisper_sched_*` metrics once real traffic exists.

### 8.3 Lease timing

- `heartbeat_interval` = 30 s, and `lease_ttl` = 3 × interval = 90 s. A worker is presumed gone after 3 missed heartbeats. Both values are server config and are sent to the worker in each lease.
- The reaper runs every 10 s. It expires leases with `expires_at < now` (outcome `expired`) and requeues the task, or fails it when attempts are exhausted.
- **Maximum lease age.** Heartbeats cannot keep a lease alive forever: once a lease is older than max(`--max-lease-age`, `--lease-age-factor` × the task's cost in seconds) (defaults 6 h and 4; cost is audio seconds × model factor, §8.2), heartbeats get `410` and the lease expires, counting as an attempt. This bounds a hung or misbehaving worker's hold on a task while leaving slow machines ample time.
- **Server restart:** on startup, every live lease is extended by one `lease_ttl` before the reaper starts. Workers reconnecting within that window carry on uninterrupted.

### 8.4 Assembly and transcript format

When the last transcribe task of a job is done:
1. Concatenate the segments in seq order, converting to absolute times.
2. Write `blobs/{job}/transcript.txt` (and `result.json`) atomically.
3. Mark the job `done` and set `completed_at` and `compute_ms`.

Transcript format: `[mm:ss.ss] text` lines, the same shape as YouTube caption transcripts, under a provenance header:

```
# transcript-source: whisper; whisper.cpp=1.9.5; model=small.en; vad=…; language=en; transcribed=2026-11-15T14:03:11Z; job=0193…; source=https://…; ffmpeg=7.1.1; tasks=4; workers=2
[00:00.00] …
[61:12.40] …
```

If the tasks ran on different whisper.cpp versions, the header lists all of them (`whisper.cpp=1.9.5,1.9.6`). See §13.

### 8.5 Retention (reaper)

| What | When | Config |
|---|---|---|
| Original media | as soon as canonical audio is accepted, or `audio_retention` after a failed prepare | — |
| Canonical audio | `completed_at + audio_retention` (default 24 h), or `audio_retention` after job failure | `WHISPER_AUDIO_RETENTION` |
| Job, transcript and tasks | `completed_at + job_retention` (default 30 d) unless deleted earlier | `WHISPER_JOB_RETENTION` |
| Queued jobs never processed | not purged; they stay visible on the dashboard and in `whisper_queue_oldest_pending_seconds` | — |
| Pending workers | 7 d after registration (polling does not extend it, so a pending worker cannot hold a slot under `--max-pending-workers` indefinitely), with their grants and the grants scoped to them | `WHISPER_PENDING_WORKER_TTL` |
| Rejected workers | 30 d after rejection, with their grants. Until then the worker's token gets a "rejected" answer telling it to exit; afterwards the token is simply unknown | `WHISPER_REJECTED_WORKER_RETENTION` |
| Ended leases | 30 d (debugging) | — |
| Orphan `.part` files | on startup and daily | — |

Once the canonical audio is purged a job can't be retried. `GET /job/{id}` reports `media.audio_purged: true`.

Every purge, deletion and prune the reaper makes is audited with actor `system` (§5.2.1).

---

## 9. Example client integration: a batch automation client

A typical client is a batch script or pipeline step that needs transcripts and does not want to run media tools or models itself. It talks plain HTTP and is unaffected by how many workers the fleet has.

| Client step | With whisper-network |
|---|---|
| Admission (size cap, pending-job cap) | Enforced client-side before anything is sent. |
| Submit | `POST /queue/{q}` with the media file as-is, `Idempotency-Key = the client's own job id`, and metadata `{requester, session, label, source}`. Delete the local file once the upload is accepted. |
| Media conversion and sandboxing | None in the client. ffmpeg runs in a worker's prepare sandbox (§7.4). |
| Waiting | A poller runs `GET /queue/{q}?since=…` every 60 s. Nothing blocks. |
| Completion | On `done`: `GET /job/{id}/transcript` (provenance header included), hand it to the consumer, then `DELETE /job/{id}`. |
| Status | Reads the client's own job table, refreshed from the service. |
| Retry policy | Server `max_attempts`. Failures arrive as `state: failed` with `error`. |

The client gets one token with `submit` on a dedicated queue, so which workers may hear its audio is an explicit `queue_admin` decision. A bulk backlog can be submitted at `weight: low`, so interactive requests at `medium` overtake it within the same queue.

The client's host can also run a worker, so adopting the service never reduces capacity.

---|---|
| Admission (size cap, per-session pending cap) | Enforced harness-side before anything is sent. |
| Submit | `POST /queue/{q}` with the media file as-is, `Idempotency-Key = harness job id`, and metadata `{requester, session, label, source}`. Delete the local file once the upload is accepted. |
| Media conversion and sandboxing | None in the harness. ffmpeg runs in a worker's prepare sandbox (§7.4). |
| Waiting | A poller runs `GET /queue/{q}?since=…` every 60 s. Nothing blocks. |
| Completion | On `done`: `GET /job/{id}/transcript` (provenance header included), deliver it to the requesting agent, then `DELETE /job/{id}`. |
| Status tool | Reads the harness's own job table, refreshed from the service. |
| Retry policy | Server `max_attempts`. Failures arrive as `state: failed` with `error`. |

The harness gets one client token with `submit` on a dedicated queue, so which workers may hear its audio is an explicit `queue_admin` decision. A bulk backlog can be submitted at `weight: low`, so interactive requests at `medium` overtake it within the same queue.

The harness host can also run a worker, so moving to the service never reduces capacity.

---

## 10. Observability

### 10.1 Logs: JSONL for VictoriaLogs

**Format.** One JSON object per line on stdout, from zerolog (the template's logger). At startup, zerolog's global field names are set to VictoriaLogs' defaults (`zerolog.TimestampFieldName = "_time"`, `MessageFieldName = "_msg"`), so `/insert/jsonline` needs no field-mapping arguments:

| zerolog default | Emitted as | Notes |
|---|---|---|
| `time` | `_time` | RFC 3339 nanoseconds, UTC |
| `message` | `_msg` | Short human sentence |
| `level` | `level` | `debug` / `info` / `warn` / `error` (lowercase) |

Every line also carries `service="whisper-network"`, `mode` (`server`/`worker`), `version`, `host`, and on workers `worker_id` once it is known.

**Stream fields.** Set `_stream_fields=service,mode,host` at ingestion (query arg or `VL-Stream-Fields` header on the collector). These are low-cardinality fields. High-cardinality IDs (`job_id`, `lease_id`, `worker_id`) are ordinary fields, never stream fields; making them stream fields would explode VictoriaLogs' stream count.

**Event fields** are flat (no nesting), named consistently across server and worker so a single query can follow a job end to end (`job_id:0193… | sort by (_time)`):

`event`, `job_id`, `task_seq`, `task_kind`, `lease_id`, `queue`, `weight`, `worker_id`, `client_id`, `user`, `model`, `duration_ms`, `audio_s`, `bytes`, `outcome`, `error`.

**Event catalog** (`event` is a stable, dotted name; dashboards and alerts key on it, not `_msg`):

| event | level | Emitted by | Key fields |
|---|---|---|---|
| `http.request` | info (debug for heartbeats) | server | `method`, `route` (pattern, e.g. `/job/{id}`, never the raw path), `status`, `duration_ms`, `bytes_in`, `bytes_out`, `principal`, `remote` |
| `job.created` | info | server | `job_id`, `queue`, `weight`, `model`, `bytes`, `original_type`, `fast_path`, `source` |
| `job.prepared` | info | server | `job_id`, `audio_s`, `tasks`, `canonical_bytes` |
| `job.done` / `job.failed` | info / warn | server | `job_id`, `queue`, `wait_s`, `elapsed_s`, `compute_s`, `error` |
| `job.deleted` / `job.purged` | info | server | `job_id`, `actor` / `what` |
| `task.leased` | info | server | `job_id`, `task_seq`, `task_kind`, `lease_id`, `worker_id`, `queue`, `cost`, `vtime` |
| `lease.expired` / `lease.released` | warn / info | server | `lease_id`, `worker_id`, `attempt` |
| `task.done` / `task.failed` | info / warn | server + worker | `lease_id`, `compute_ms`, `audio_s`, `rtf`, `error`, `stderr_tail` (≤ 4 KiB) |
| `worker.registered` / `.approved` / `.paused` / `.resumed` / `.rejected` | info | server | `worker_id`, `hostname`, `actor`, `queues` |
| `worker.sandbox` | info / warn | worker | `sandbox_level`, `landlock_abi` |
| `auth.denied` | warn | server | `principal`, `route`, `reason` |
| `admin.change` | info | server | `actor`, `action`, `target` (mirrors the audit table) |
| `reaper.run` | debug | server | counts per action |

**Never logged:** tokens, passwords, cookies, `Authorization` headers, prompt text, transcript text, or the contents of `metadata` (which carries requester names). Their lengths may be logged. `source` URLs are logged, because they are the redo key and are not secret.

**Shipping.** stdout only, and a collector ships it. That might be vlagent, Vector, Fluent Bit or the Docker log driver; it sets `_stream_fields`. Workers on ad-hoc machines may have no collector at all. That is acceptable, because everything about a task that matters fleet-wide is already logged by the server: lease events, results, and failures with the stderr tail. Direct push from the binary (`--log-push-url`) is deferred until a fleet machine actually needs it.

Flags come from the template: `--debug` / `--quiet` set the level (default `info`), and `--output-format auto|jsonl|terminal` chooses the format. `auto` gives human-readable output on a TTY and JSONL otherwise, so containers emit JSONL with no configuration.

### 10.2 Metrics: VictoriaMetrics and Prometheus

**Library:** `github.com/VictoriaMetrics/metrics`.
- It writes Prometheus text exposition, so both Prometheus and VictoriaMetrics can scrape it.
- `PrometheusHistogram` produces standard `le` buckets, so `histogram_quantile` works in either system.
- Push is built in (`InitPushWithOptions`, with extra labels and headers) to VictoriaMetrics' `/api/v1/import/prometheus`.

It was chosen over `prometheus/client_golang` because push is native, and it is smaller. The trade-off is that it lacks client_golang's collector ecosystem, which isn't needed here. Confidence: medium-high.

**Server**
- **Scrape:** `GET /metrics` on a separate listener, `--metrics-listen` (default `:9090`), so metrics are never exposed on the public port and need no auth on a private network.
- **Push (optional):** `--metrics-push-url` (e.g. `http://vm:8428/api/v1/import/prometheus`), `--metrics-push-interval` (default 15 s), and `--metrics-labels` for extra `k="v"` labels.

**Workers: zero-config by default**
- Fleet metrics are derived on the server from the protocol, so workers need no metrics config to be visible. The server already knows every lease, result, compute time and realtime factor.
- Heartbeat bodies carry a few worker gauges, which the server re-exports with a `worker` label: slots busy, child RSS, load average, and cgroup CPU throttling.
- **Optional, for host detail:** `--metrics-push-url` (same semantics as the server) or `--metrics-listen` for scraping.

**Cardinality rules**
- Never label by job, lease or client.
- `worker` (ID) is allowed; the fleet is expected to stay under ~100 workers. `whisper_worker_info` carries the descriptive labels, which can be joined in queries.
- `route` uses the pattern, not the path.

**Metric catalog**

| Metric | Type | Labels | Why |
|---|---|---|---|
| `whisper_jobs` | gauge | `queue`, `state` | Backlog shape |
| `whisper_tasks` | gauge | `queue`, `kind`, `state` | |
| `whisper_queue_backlog_cost` | gauge | `queue` | Pending cost: the work remaining, in model-weighted audio seconds |
| `whisper_queue_oldest_pending_seconds` | gauge | `queue` | Primary alert signal |
| `whisper_queue_weight` | gauge | `queue` | For share calculations |
| `whisper_sched_served_cost_total` | counter | `queue`, `class` | Fairness check: `rate(served)` against weight |
| `whisper_sched_vtime` | gauge | `queue` | Debugging the scheduler |
| `whisper_jobs_created_total` | counter | `queue`, `weight`, `fast_path` | |
| `whisper_jobs_finished_total` | counter | `queue`, `outcome` | |
| `whisper_job_wait_seconds` / `whisper_job_elapsed_seconds` | histogram | `queue` | Latency as seen by clients |
| `whisper_task_compute_seconds` | histogram | `kind`, `model` | |
| `whisper_task_realtime_factor` | histogram | `model`, `worker` | Calibrates `model_factor` and slot sizing |
| `whisper_leases_ended_total` | counter | `outcome`, `kind` | Expiry rate means sick workers or a TTL that is too short |
| `whisper_claim_wait_seconds` | histogram | — | Idle fleet vs starved fleet |
| `whisper_workers` | gauge | `state` | |
| `whisper_worker_info` | gauge (=1) | `worker`, `hostname`, `description`, `engine_version`, `ffmpeg_version`, `sandbox_level`, `arch` | Join target |
| `whisper_worker_last_seen_timestamp_seconds` | gauge | `worker` | Dead-worker alert |
| `whisper_worker_slots` / `whisper_worker_slots_busy` | gauge | `worker` | Utilization |
| `whisper_worker_audio_seconds_total` | counter | `worker`, `model` | Throughput per machine |
| `whisper_worker_child_rss_bytes` | gauge | `worker` | Memory sizing (§7.3) |
| `whisper_http_requests_total` | counter | `route`, `method`, `code` | |
| `whisper_http_request_duration_seconds` | histogram | `route` | |
| `whisper_upload_bytes_total` / `whisper_input_served_bytes_total` | counter | `queue` / `kind` | Bandwidth |
| `whisper_storage_bytes` | gauge | `kind` (`original`, `canonical`, `transcript`) | |
| `whisper_data_free_bytes` | gauge | — | Disk alert |
| `whisper_db_tx_seconds` | histogram | `op` | SQLite contention, if it ever appears |
| `go_*`, `process_*` | — | — | From the library |

**Suggested alerts** (vmalert / Prometheus rules, shipped as `deploy/alerts.yml`)

| Alert | Condition |
|---|---|
| Queue stalled | `whisper_queue_oldest_pending_seconds > 6h` and `whisper_queue_backlog_cost > 0` |
| Lease churn | `rate(whisper_leases_ended_total{outcome="expired"}[15m]) > 0.1` |
| Worker vanished | active worker with `time() - whisper_worker_last_seen_timestamp_seconds > 600` |
| Disk | `whisper_data_free_bytes / (whisper_data_free_bytes + sum(whisper_storage_bytes)) < 0.1` |
| No eligible worker | `whisper_tasks{state="pending"} > 0` and no worker granted that queue has been seen in 10 min (computed server-side as `whisper_queue_eligible_workers == 0`) |

### 10.3 Health

`GET /healthz` (process up) and `GET /readyz` (DB writable, data dir free space above threshold), both on the main listener and unauthenticated.

---

## 11. Operations

### 11.1 Configuration

Every flag has an env var equivalent (`WHISPER_*`). There is no config file in v1.

**Server**

| Flag | Default | Notes |
|---|---|---|
| `--listen` | `:8080` | |
| `--metrics-listen` | `127.0.0.1:9090` | Loopback by default; empty disables it |
| `--metrics-push-url` / `--metrics-push-interval` / `--metrics-labels` | — / 15s / — | |
| `--debug` / `--quiet` / `--output-format` | off / off / `auto` | Template flags (§10.1) |
| `--data` | `/data` | SQLite file plus `blobs/`, `models/` |
| `--public-url` | unset (relative paths) | External base URL for `Location` headers and `/llms.txt`. Never applied to the worker protocol: lease `input_url`s stay relative so workers may reach the server at a different address |
| `--tls-cert/--tls-key` | — | If unset, plain HTTP; the server logs a warning when not bound to localhost |
| `--require-tls` | off | Refuse to start without TLS |
| `--trust-proxy` | off | Trust `X-Forwarded-For` (rightmost entry) for rate limiting and audit; only behind a proxy that sets it |
| `--heartbeat-interval` | 30s | Leases live 3× this |
| `--max-attempts` | 2 | |
| `--task-target` | 60m | |
| `--split-window` | 60s | Search window around each split point |
| `--class-weights` | `low=1,medium=2,high=4` | |
| `--model-factors` | `small.en=1,large-v3-turbo=3` | |
| `--prepare-min-sandbox` | `full` | `full` / `fs-only` / `none` |
| `--max-upload-bytes` | 2 GiB | A queue may set a lower cap; a queue cap above this is refused (400), and uploads are clamped to it regardless |
| `--min-free-bytes` | 1 GiB | Uploads that would take free disk below this are refused (507) |
| `--audio-retention` / `--job-retention` | 24h / 720h | §8.5 |
| `--pending-worker-ttl` | 168h | Pending workers are deleted this long after registering (§8.5) |
| `--rejected-worker-retention` | 720h | Rejected workers are deleted this long after rejection (§8.5) |
| `--max-pending-workers` | 100 | Cap on workers awaiting approval; registration beyond it is 503 |
| `--max-pending-per-source` | 3 | Cap on workers awaiting approval registered from one address (IPv4 address or IPv6 /64); beyond it registration is 429 |
| `--max-uploads-per-principal` | 2 | Job uploads one client or user may have in progress at once; more get 429 with `Retry-After` |
| `--max-analyses-per-principal` | 1 | Media analyses (validation, encoding) one principal may run at once; more wait up to 30 s, then get 429 |
| `--max-lease-age` / `--lease-age-factor` | 6h / 4 | Heartbeats stop extending a lease older than max(age, factor × task cost in seconds) (§8.3) |
| `--models` | `small.en,large-v3-turbo` | Allowlist plus pinned sha256 (built-in table) |
| `--vad-model` | `silero-v5.1.2` | VAD model sent with transcribe leases; empty disables |
| `--model-upstream` | built-in | Base URL to fetch model files from instead |
| `--max-claim-wait` | 60s | Cap on a worker claim's long-poll |
| `--session-ttl` | 12h | UI session lifetime |
| `--shutdown-timeout` | 15s | Graceful shutdown limit |
| `--read-timeout` | 60s | Limit on reading a request (headers and small bodies); a streaming upload must instead keep making progress at least this often |
| `--min-transfer-rate` | 64 KiB | Slowest accepted upload or download rate per second; sizes the deadlines of large transfers, and a streaming upload must sustain it over every sliding window of max(`--read-timeout`, 30s) (§6.1) |
| `--password-memory-budget` | 512 MiB | Memory for concurrent argon2id hashing, which bounds parallel logins and password changes; beyond it requests wait, then get 503 with `Retry-After` |
| `--admin-password` | random | Used only on first start |

**Worker**

| Flag | Default | Notes |
|---|---|---|
| `--server` | **required** | The only required setting |
| `--state` | `/var/lib/whisper-network` | Credentials and model cache. Mount as a volume. |
| `--description` | hostname | |
| `--threads-per-slot` | 4 | |
| `--slots` | derived (§7.3) | Override |
| `--memory-reserve` | 1 GiB | |
| `--kinds` | `prepare,transcribe` | e.g. `transcribe` only, to keep ffmpeg off a machine |
| `--whisper-cli` / `--ffmpeg` | on `$PATH` | |
| `--drain` | false | Graceful shutdown finishes running tasks first |
| `--debug` / `--quiet` / `--output-format` | off / off / `auto` | Template flags (§10.1) |
| `--metrics-listen` / `--metrics-push-url` | — / — | Optional (§10.2) |

### 11.2 Deployment

```
docker run -d -v wn-data:/data -p 8080:8080 -p 127.0.0.1:9090:9090 \
           ghcr.io/…/whisper-network server
docker run -d -v wn-state:/var/lib/whisper-network --cpu-shares 128 --memory 6g \
           ghcr.io/…/whisper-network worker --server https://wn.example
```

- The server should sit behind a TLS-terminating proxy (Traefik or Caddy) or use `--tls-*`. Worker and client tokens must not cross untrusted networks in cleartext.
- Backups: `sqlite3 .backup` (or `VACUUM INTO`) plus the `blobs/` directory. Media is transient, so backing up transcripts and the DB is enough.
- `deploy/` ships an example compose file with VictoriaMetrics, VictoriaLogs and vmalert, wired to the flags above. It doubles as the integration-test environment.

### 11.3 Capacity sanity check

Server load is trivial. 50 workers × 4 slots heartbeating every 30 s is about 7 writes/s, and SQLite in WAL mode handles thousands. The binding constraints are disk for media and the uplink to workers:
- Disk for a hypothetical 500 h backlog is ~25–35 GB of canonical FLAC, plus originals until each is prepared. Bounded by drain rate; watched by the disk alert.
- Upload of video originals to prepare workers is one-time. WAV to transcribe workers is ~0.1 MB/s per slot (§8.1).

---

## 12. Testing strategy

| Layer | Approach |
|---|---|
| `media` | Table tests: valid and invalid WAV and FLAC headers (wrong rate, stereo, float, truncated, oversize chunk lengths), split-point selection on synthetic PCM with planted silences, and a FLAC range → WAV round trip that compares samples. Fuzz both validators (`go test -fuzz`). |
| `sched` | Pure-function tests: two- and three-queue simulations assert that served-cost ratios converge to the weights within one task cost. Covers reactivation (an idle queue does not bank credit), class shares inside a queue, eligibility subsets, and weight changes mid-run. |
| `store` | Runs against a real SQLite file in `t.TempDir()`; no mocks. The **claim race test** runs 200 goroutines claiming from 50 tasks and asserts that no task ever has two live leases. Lease expiry and requeue use an injected clock. |
| `auth` | A table-driven authorization matrix over principal × role × scope × action → allow/deny/404. This is the main guard against privilege bugs. |
| API | `httptest.Server` end-to-end: register → approve → upload (fast path and prepare path) → claim → heartbeat → result → assembly → transcript. Fencing: an expired lease's `PUT result` gets 410 and does not change the stored result. |
| Worker | A fake `whisper-cli`/`ffmpeg` (a test helper binary via `TestMain` re-exec) that emits canned output, sleeps, crashes or hangs on cue. Covers heartbeat-driven cancellation, SIGTERM release, and resuming an interrupted download. |
| Sandbox | Linux-only tests: the child cannot read `credentials.json`, cannot open TCP (on ABI ≥ 4), and cannot write outside tmp. Skipped with a reason where the kernel lacks support. |
| Observability | A golden test captures the log output of a full job run and asserts the event names and required fields, and that no forbidden field (token, prompt, transcript) appears. A metrics test scrapes `/metrics` and asserts the presence and labels of the catalog. |
| Integration | The `deploy/` compose stack: one server, two workers, real ffmpeg and whisper.cpp on a 2-minute MP4, with VictoriaLogs/VictoriaMetrics receiving data. Runs nightly in CI, not per-PR. |

Code is structured for testability: clock, ID generator, process runner and scheduler snapshot are interfaces or values injected at construction. There are no package-level globals; the metrics `Set` is injected too, rather than using the library's default global set.

---

## 13. Security summary

| Threat | Mitigation |
|---|---|
| Malicious media exploits ffmpeg | ffmpeg runs only in worker prepare tasks under `sandbox-exec` (§7.4): Landlock filesystem rules and IPC scoping, a seccomp filter that refuses `socket` and `socketpair`, `io_uring`, new namespaces and leaving the process group the worker kills, `no_new_privs`, no capabilities, an allow-listed environment, a dedicated unprivileged UID when the worker is root, rlimits (incl. a shared `RLIMIT_NPROC`) and a timeout. ffmpeg runs with `-loglevel error`, so the input's metadata stays out of logs. The worker creates child files with `O_EXCL|O_NOFOLLOW`, reads outputs without following links, refuses cross-origin redirects, and caps input downloads at the lease's size (refusing leases without one). Prepare tasks go only to workers whose self-test (file read, TCP, MPTCP, UDP, pathname and abstract Unix sockets, Unix datagram via `socketpair`, signals) reaches `--prepare-min-sandbox`. **Residual:** children of one worker share a uid and its process allowance; architectures without the seccomp filter, and kernels without Landlock, report a weaker level. |
| Malicious canonical audio from a compromised worker | Validated by a full pure-Go decode with size and duration bounds. Memory-safe, so it can at worst fail validation or waste CPU. |
| Malicious media exploits whisper-cli | whisper-cli only ever receives server-generated PCM WAV, and runs under the same sandbox as ffmpeg. |
| Rogue worker registers and harvests media | Registration grants nothing. The `pending` state and per-queue `process` grants are explicit human decisions, and every approval is audited. No enrollment tokens. |
| Registration spam | Per-IP rate limit, a cap on total pending workers (default 100), and a TTL reaper. |
| Stale or partitioned worker overwrites a newer result | Lease fencing; non-live leases get 410. |
| Job ID enumeration | UUIDv7 IDs plus 404 for jobs the caller can't see. UUIDv7 is time-ordered and partly predictable, so authorization, not secrecy, is the guarantee. |
| Default credentials | Random first-run admin password; change forced at first login. |
| Token theft from the DB | Only sha256 hashes are stored. Passwords use argon2id. |
| Secrets or PII in logs | Explicit never-log list (§10.1), enforced by a golden test. |
| Metrics exposure | Separate listener, bound privately by default in the examples. |
| CSRF / session fixation | SameSite cookies, a per-session CSRF token, and session rotation on login. |
| Privilege escalation via `POST /client` | Grants handed out must be a subset of the caller's own. |
| Queue owner grabs fleet share | Only `admin` can change queue weights; changes are audited. |
| Cleartext tokens | TLS required outside localhost (warning plus docs). Optional `--require-tls` refuses to start without it. |
| Disk exhaustion | Per-queue upload caps, `/readyz` free-space check, refusing new uploads below a threshold (`507`), and a disk alert. |
| Model-file tampering | Pinned sha256 per model, verified on the server before caching and on the worker before use. |

---

## 14. Open questions

1. **Mixed engine versions in one job.** Accept and record them (as specified above), or pin a job to the first version that touches it? Recommendation: accept and record.
2. **Lease cost refunds.** Should cost charged to an expired or failed lease be refunded? Recommendation: no, unless the `whisper_leases_ended_total{outcome="expired"}` rate shows it matters.
3. **Changing a job's weight after submission** (`PATCH /job/{id} {"weight":"high"}` by the owner while it is queued). Cheap to add. Recommendation: add it when someone asks.
4. **Auto-calibrating `model_factor`** from the `whisper_task_realtime_factor` metric instead of static config. Recommendation: static for v1, revisit with data.
5. **Multiple audio streams** in an upload (e.g. language tracks). v1 takes the first. A `params.audio_stream` index is the obvious extension.
6. **`delete_all` client role.** Is it needed, or is "owner or queue_admin" enough?
7. **Result callbacks.** Webhooks would remove polling, but they don't work for clients behind NAT. Polling is enough until a client needs push.
8. **Re-transcription with a better model** while the audio is retained. Recommendation: client-side resubmit; YAGNI.

Resolved in r2: fairness (§8.2), FLAC (§8.1.1), enrollment tokens (deferred: explicit approval only).

---

## 15. Implementation plan

Logging and metrics go in from phase 1, because instrumenting as each piece is written is cheaper than retrofitting.

| Phase | Scope | Exit criterion |
|---|---|---|
| 0 | Repo skeleton, `cmd/whisper-network`, `internal/{store,media,sched,auth,obs,server,worker,ui}`, migrations, CI (lint, test, fuzz smoke) | `go test ./...` green |
| 1 | Store, WAV/FLAC validate/split/serve, weighted fair claim, client API, worker API, leases, reaper, JSONL logging, `/metrics`. Tokens bootstrapped via CLI (`whisper-network admin create-client`). | E2E test: canonical upload → 2 fake workers → transcript; sched simulation tests pass; log golden test passes |
| 2 | Real whisper-cli runner, prepare via ffmpeg, `sandbox-exec` (Landlock, UID, rlimits), model cache, `bench`, metrics push, heartbeat gauges, container images (amd64/arm64), darwin binary | MP4 → transcript on two real machines; sandbox tests pass on the fleet's kernels; bench numbers recorded and §7.3 defaults updated |
| 3 | UI: login, worker approval, queues and weights, jobs, users/clients, audit, share dashboard | Worker approved and job run without touching the CLI |
| 4 | `deploy/` compose stack with VictoriaMetrics/VictoriaLogs/vmalert and alert rules; nightly integration test | Alerts fire in a staged failure (killed worker, full disk) |
| 5 | First production client integration (§9): validate on a sample, then submit the full backlog at `low` | Sample transcripts are complete and correctly labelled |
