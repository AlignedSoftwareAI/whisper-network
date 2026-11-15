// Package api defines the JSON wire contract shared by the server, the worker
// and the Go client. It contains only data types and constants: no behaviour,
// no dependencies beyond the standard library. See DESIGN.md §6.
package api

import (
	"encoding/json"
	"time"
)

// JobState is the client-visible lifecycle state of a job (DESIGN.md §4.2).
type JobState string

const (
	JobPreparing JobState = "preparing"
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobDone      JobState = "done"
	JobFailed    JobState = "failed"
)

// TaskState is the internal lifecycle state of a task.
type TaskState string

const (
	TaskPending TaskState = "pending"
	TaskLeased  TaskState = "leased"
	TaskDone    TaskState = "done"
	TaskFailed  TaskState = "failed"
)

// TaskKind distinguishes media normalization from transcription.
type TaskKind string

const (
	KindPrepare    TaskKind = "prepare"
	KindTranscribe TaskKind = "transcribe"
)

// Weight is a job's weight class within its queue (DESIGN.md §8.2).
type Weight string

const (
	WeightLow    Weight = "low"
	WeightMedium Weight = "medium"
	WeightHigh   Weight = "high"
)

// Valid reports whether w is one of the defined weight classes.
func (w Weight) Valid() bool {
	return w == WeightLow || w == WeightMedium || w == WeightHigh
}

// WorkerState is the approval lifecycle state of a worker.
type WorkerState string

const (
	WorkerPending  WorkerState = "pending"
	WorkerActive   WorkerState = "active"
	WorkerPaused   WorkerState = "paused"
	WorkerRejected WorkerState = "rejected"
)

// SandboxLevel is how strongly a worker can confine its child processes
// (DESIGN.md §7.4). Ordered: None < FSOnly < Full.
type SandboxLevel string

const (
	SandboxNone   SandboxLevel = "none"
	SandboxFSOnly SandboxLevel = "fs-only"
	SandboxFull   SandboxLevel = "full"
)

// Rank orders sandbox levels so policies can compare them. Unknown levels rank
// below None.
func (l SandboxLevel) Rank() int {
	switch l {
	case SandboxNone:
		return 0
	case SandboxFSOnly:
		return 1
	case SandboxFull:
		return 2
	}
	return -1
}

// LeaseOutcome records how a lease ended.
type LeaseOutcome string

const (
	OutcomeDone      LeaseOutcome = "done"
	OutcomeExpired   LeaseOutcome = "expired"
	OutcomeReleased  LeaseOutcome = "released"
	OutcomeFailed    LeaseOutcome = "failed"
	OutcomeCancelled LeaseOutcome = "cancelled"
)

// Role names used in grants (DESIGN.md §5.2).
type Role string

const (
	RoleAdmin       Role = "admin"
	RoleQueueAdmin  Role = "queue_admin"
	RoleWorkerAdmin Role = "worker_admin"
	RoleClientAdmin Role = "client_admin"
	RoleSubmit      Role = "submit"
	RoleViewAll     Role = "view_all"
	RoleDeleteAll   Role = "delete_all"
	RoleProcess     Role = "process"
)

// Grant is a role bound to a scope. Scope is "*", "queue:<name>" or
// "worker:<id>" on the wire (queues are addressed by name, not internal id).
type Grant struct {
	Role  Role   `json:"role"`
	Scope string `json:"scope"`
}

// ---------------------------------------------------------------------------
// Client API (§6.1)

// JobParams is the "params" multipart part of POST /queue/{q}.
type JobParams struct {
	Model    string          `json:"model"`
	Language string          `json:"language,omitempty"` // default "en"
	Weight   Weight          `json:"weight,omitempty"`   // default medium
	Prompt   string          `json:"prompt,omitempty"`   // <= MaxPromptLen chars
	Label    string          `json:"label,omitempty"`
	Source   string          `json:"source,omitempty"`   // recorded, never fetched
	Metadata json.RawMessage `json:"metadata,omitempty"` // opaque, <= MaxMetadataBytes
}

const (
	MaxPromptLen     = 800
	MaxMetadataBytes = 8 << 10
	MaxLabelLen      = 200
	MaxSourceLen     = 2048
)

// MediaInfo describes a job's media (§6.1 job status).
type MediaInfo struct {
	OriginalBytes  int64   `json:"original_bytes"`
	OriginalType   string  `json:"original_type,omitempty"`
	DurationS      float64 `json:"duration_s,omitempty"`
	OriginalPurged bool    `json:"original_purged"`
	AudioPurged    bool    `json:"audio_purged"`
}

// TaskCounts summarizes a job's tasks by state.
type TaskCounts struct {
	Total   int `json:"total"`
	Pending int `json:"pending"`
	Leased  int `json:"leased"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
}

// Downloads records transcript retrieval.
type Downloads struct {
	Count int        `json:"count"`
	First *time.Time `json:"first"`
	Last  *time.Time `json:"last"`
}

// Job is the job status document returned by GET /job/{id} and list endpoints.
type Job struct {
	ID          string          `json:"id"`
	Queue       string          `json:"queue"`
	State       JobState        `json:"state"`
	Weight      Weight          `json:"weight"`
	Model       string          `json:"model"`
	Language    string          `json:"language"`
	Prompt      string          `json:"prompt,omitempty"`
	Label       string          `json:"label,omitempty"`
	Source      string          `json:"source,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Media       MediaInfo       `json:"media"`
	Tasks       TaskCounts      `json:"tasks"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   *time.Time      `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at"`
	WaitS       *float64        `json:"wait_s"`
	ElapsedS    *float64        `json:"elapsed_s"`
	ComputeS    float64         `json:"compute_s"`
	Downloads   Downloads       `json:"downloads"`
	Error       *string         `json:"error"`
}

// JobList is the response of GET /queue/{q}.
type JobList struct {
	Jobs       []Job  `json:"jobs"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Queue is the representation of a queue for admin endpoints and the UI.
type Queue struct {
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Weight         int    `json:"weight"`
	MaxUploadBytes int64  `json:"max_upload_bytes,omitempty"`
}

// QueuePatch is the body of PATCH /queue/{q}. Nil fields are unchanged.
type QueuePatch struct {
	Description    *string `json:"description,omitempty"`
	Weight         *int    `json:"weight,omitempty"`
	MaxUploadBytes *int64  `json:"max_upload_bytes,omitempty"`
}

// CreateClientRequest is the body of POST /client.
type CreateClientRequest struct {
	Name   string  `json:"name"`
	Grants []Grant `json:"grants"`
}

// Client is the representation of an API client. Token is set only in the
// response that created it.
type Client struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Grants     []Grant    `json:"grants"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Token      string     `json:"token,omitempty"`
}

// Segment is one transcribed span. Times are milliseconds; relative to the
// task start in a TranscribeResult, absolute in GET /job/{id}/result.
type Segment struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

// JobResult is GET /job/{id}/result.
type JobResult struct {
	JobID      string    `json:"job_id"`
	Provenance string    `json:"provenance"`
	Segments   []Segment `json:"segments"`
}

// ---------------------------------------------------------------------------
// Worker API (§6.2)

// WorkerFacts is what a worker reports about itself at registration and on
// PATCH /worker/{id}.
type WorkerFacts struct {
	Hostname      string       `json:"hostname"`
	Description   string       `json:"description,omitempty"`
	CPUModel      string       `json:"cpu_model,omitempty"`
	Cores         int          `json:"cores"`
	MemBytes      int64        `json:"mem_bytes"`
	OS            string       `json:"os"`
	Arch          string       `json:"arch"`
	EngineVersion string       `json:"engine_version,omitempty"`
	FFmpegVersion string       `json:"ffmpeg_version,omitempty"`
	SandboxLevel  SandboxLevel `json:"sandbox_level"`
	Kinds         []TaskKind   `json:"kinds"`
	Slots         int          `json:"slots"`
	Models        []string     `json:"models"`
	Version       string       `json:"version,omitempty"` // whisper-network version
}

// RegisterResponse is returned by POST /worker. Token is shown once.
type RegisterResponse struct {
	ID    string      `json:"id"`
	Token string      `json:"token"`
	State WorkerState `json:"state"`
}

// Worker is GET /worker/{id}.
type Worker struct {
	ID         string      `json:"id"`
	State      WorkerState `json:"state"`
	Facts      WorkerFacts `json:"facts"`
	Queues     []string    `json:"queues"` // queues this worker may process
	LastSeenAt *time.Time  `json:"last_seen_at,omitempty"`
	// Hints from the server.
	HeartbeatIntervalS int `json:"heartbeat_interval_s"`
	PollIntervalS      int `json:"poll_interval_s"`
}

// ClaimRequest is the body of POST /worker/{id}/leases.
type ClaimRequest struct {
	Kinds  []TaskKind `json:"kinds"`
	Models []string   `json:"models"`
}

// Lease is the claim response (201).
type Lease struct {
	ID                 string    `json:"id"`
	Kind               TaskKind  `json:"kind"`
	Job                string    `json:"job"`
	Seq                int       `json:"seq"`
	InputURL           string    `json:"input_url"`
	InputBytes         int64     `json:"input_bytes,omitempty"`  // prepare: original size
	InputSHA256        string    `json:"input_sha256,omitempty"` // prepare: original hash
	AudioSamples       int64     `json:"audio_samples,omitempty"`
	Model              string    `json:"model,omitempty"`
	ModelSHA256        string    `json:"model_sha256,omitempty"`
	VADModel           string    `json:"vad_model,omitempty"`
	VADModelSHA256     string    `json:"vad_model_sha256,omitempty"`
	Language           string    `json:"language,omitempty"`
	Prompt             string    `json:"prompt,omitempty"`
	HeartbeatIntervalS int       `json:"heartbeat_interval_s"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// Gauges are worker readings carried on heartbeats (§10.2).
type Gauges struct {
	SlotsBusy        int     `json:"slots_busy"`
	ChildRSSBytes    int64   `json:"child_rss_bytes"`
	Load1            float64 `json:"load1"`
	CPUThrottledSecs float64 `json:"cpu_throttled_seconds"`
}

// HeartbeatRequest is the optional body of POST /lease/{id}/heartbeat.
type HeartbeatRequest struct {
	Gauges *Gauges `json:"gauges,omitempty"`
}

// HeartbeatResponse is the 200 body of a heartbeat.
type HeartbeatResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// TranscribeResult is the body of PUT /lease/{id}/result for transcribe
// leases. (Prepare leases PUT the canonical FLAC as audio/flac instead.)
type TranscribeResult struct {
	EngineVersion string    `json:"engine_version"`
	Model         string    `json:"model"`
	ModelSHA256   string    `json:"model_sha256"`
	VAD           string    `json:"vad,omitempty"`
	Language      string    `json:"language"`
	ComputeMS     int64     `json:"compute_ms"`
	Segments      []Segment `json:"segments"`
}

// Prepare result headers (PUT /lease/{id}/result with Content-Type audio/flac).
const (
	HeaderComputeMS     = "X-Compute-Ms"
	HeaderFFmpegVersion = "X-Ffmpeg-Version"
)

// FailureReport is the body of POST /lease/{id}/failure.
type FailureReport struct {
	Reason     string `json:"reason"`
	StderrTail string `json:"stderr_tail,omitempty"` // <= MaxStderrTail bytes
	Retryable  bool   `json:"retryable"`
}

const MaxStderrTail = 4 << 10

// ---------------------------------------------------------------------------
// Errors (RFC 9457)

// Problem is an application/problem+json error body.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Extension members.
	State string `json:"state,omitempty"` // e.g. worker state on 403/409
}

// Stable problem type slugs.
const (
	ProblemBadRequest     = "bad-request"
	ProblemUnauthorized   = "unauthorized"
	ProblemForbidden      = "forbidden"
	ProblemNotFound       = "not-found"
	ProblemConflict       = "conflict"
	ProblemLeaseGone      = "lease-gone"
	ProblemWorkerPending  = "worker-pending"
	ProblemWorkerPaused   = "worker-paused"
	ProblemWorkerRejected = "worker-rejected"
	ProblemMediaInvalid   = "media-invalid"
	ProblemTooLarge       = "too-large"
	ProblemResultInvalid  = "result-invalid"
	ProblemNotDone        = "not-done"
	ProblemStorageFull    = "storage-full"
	ProblemRateLimited    = "rate-limited"
	ProblemInternal       = "internal"
)

// ProblemContentType is the media type for Problem bodies.
const ProblemContentType = "application/problem+json"

// Token prefixes (§5.1).
const (
	ClientTokenPrefix = "wnc_"
	WorkerTokenPrefix = "wnw_"
)
