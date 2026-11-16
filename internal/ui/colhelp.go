package ui

import (
	"fmt"
	"strings"
)

// Every table column explains itself (#79, DESIGN.md §6.3): its header
// carries the help text as a hover tooltip and as an ⓘ button that shows the
// same text, for keyboards, touch screens and screen readers. The "th"
// template renders such a header from a column; columns is the one source of
// the text, so a page cannot ship a header without help (the column lookup
// fails the render, and a test walks every page's headers).

// column is one table header and its explanation.
type column struct {
	ID    string // DOM id of the help text, unique per page
	Label string
	Help  string
	Class string // the cell class, e.g. "n" for numbers
	More  string // link to the fuller explanation, if any
}

// designDoc is where "more…" links point: the design document in the
// project repository.
const designDoc = "https://github.com/deweysasser/whisper-network/blob/main/DESIGN.md"

// columns maps a column key ("page.column") to its header and help.
var columns = map[string]column{
	// Dashboard: queues.
	"dash.queue":     {Label: "Queue", Help: "The queue; open it for its jobs and pending detail."},
	"dash.preparing": {Label: "Preparing", Help: "Jobs whose upload is being converted to canonical audio (a prepare task)."},
	"dash.queued":    {Label: "Queued", Help: "Jobs ready to transcribe that no worker has started yet."},
	"dash.running":   {Label: "Running", Help: "Jobs with at least one task started."},
	"dash.done":      {Label: "Done", Help: "Jobs with a finished transcript."},
	"dash.failed":    {Label: "Failed", Help: "Jobs that failed for good; they can be retried while their media is kept."},
	"dash.pending":   {Label: "Pending tasks", Help: "Tasks waiting for a worker."},
	"dash.leased":    {Label: "Leased", Help: "Tasks a worker is running now."},
	"dash.oldest": {Label: "Oldest pending", Help: "How long the queue's oldest job with a pending task has existed.",
		More: designDoc + "#102-metrics-victoriametrics-and-prometheus"},
	"dash.eligible": {Label: "Eligible workers", Help: "Active workers seen in the last few minutes that may process this queue. Zero while tasks are pending means nothing can take them."},
	"dash.slots": {Label: "Eligible slots", Help: "Slots in use / total on those workers, from their last heartbeat. All slots in use: the queue is waiting for capacity; slots free: it is waiting its turn or for a matching worker.",
		More: designDoc + "#63-ui-ui"},
	// Dashboard: fleet share.
	"share.queue":  {Label: "Queue", Help: "The queue."},
	"share.weight": {Label: "Weight", Help: "The queue's share of the fleet relative to other queues; only an admin sets it.", More: designDoc + "#82-scheduling-weighted-fair-queueing"},
	"share.configured": {Label: "Configured", Help: "The queue's weight over the total weight of queues that have work: the share the scheduler aims for.",
		More: designDoc + "#82-scheduling-weighted-fair-queueing"},
	"share.actual": {Label: "Actual", Help: "The share of scheduling cost (model-weighted audio seconds) the queue was actually served recently."},

	// Queues list.
	"queues.name":   {Label: "Name", Help: "The queue's name, used in API paths."},
	"queues.desc":   {Label: "Description", Help: "What the queue is for, set by its queue admin."},
	"queues.weight": {Label: "Weight", Help: "The queue's share of the fleet relative to other queues (default 100).", More: designDoc + "#82-scheduling-weighted-fair-queueing"},
	"queues.cap":    {Label: "Upload cap", Help: "The largest upload the queue accepts; default is the server's limit."},
	"queues.jobs":   {Label: "Jobs", Help: "Whether you may see the queue's jobs."},

	// Queue page: jobs.
	"queue.select":  {Label: "Select", Help: "Choose jobs for a bulk retry or delete."},
	"queue.job":     {Label: "Job", Help: "The job's label (else its source, else its id), with the end of its id; opens the job."},
	"queue.state":   {Label: "State", Help: "preparing: converting the upload; queued: waiting for a worker; running: tasks in progress; done; failed."},
	"queue.weight":  {Label: "Weight", Help: "The job's priority class within the queue: high, medium or low.", More: designDoc + "#82-scheduling-weighted-fair-queueing"},
	"queue.model":   {Label: "Model", Help: "The whisper model the job is transcribed with."},
	"queue.audio":   {Label: "Audio", Help: "The length of the job's audio, once prepared."},
	"queue.tasks":   {Label: "Tasks", Help: "Task counts: done of total, then pending, leased (running) and failed ones. \"waiting\" is how long pending tasks have waited with none running."},
	"queue.workers": {Label: "Workers", Help: "Workers running the job's tasks now, or that finished them. Workers you may not see are counted as \"running\"."},
	"queue.created": {Label: "Created", Help: "When the job was submitted."},
	// Queue page: pending summary.
	"pending.kind":   {Label: "Kind", Help: "prepare converts an upload to canonical audio; transcribe runs whisper on a span of it."},
	"pending.model":  {Label: "Model", Help: "The whisper model the tasks need; a worker must offer it."},
	"pending.weight": {Label: "Weight", Help: "The jobs' priority class within the queue."},
	"pending.tasks":  {Label: "Tasks", Help: "Pending tasks in this group."},
	"pending.audio":  {Label: "Audio", Help: "Audio still to transcribe in this group."},
	"pending.oldest": {Label: "Oldest wait", Help: "How long the group's longest-waiting task has been pending."},
	"pending.why": {Label: "Why waiting", Help: "Why no eligible worker can take these tasks right now, best effort. Empty: they are waiting their turn.",
		More: designDoc + "#63-ui-ui"},
	// Queue page: access.
	"access.principal": {Label: "Principal", Help: "Who holds the role: a user, an API client or a worker."},
	"access.role":      {Label: "Role", Help: "What the role allows on this queue.", More: designDoc + "#5-authorization"},
	"access.actions":   {Label: "Actions", Help: "Revoke a grant you could have given."},

	// Job page.
	"task.seq":       {Label: "#", Help: "The task's number in the job: 0 is prepare, 1 and up are transcribe spans."},
	"task.kind":      {Label: "Kind", Help: "prepare converts the upload to canonical audio; transcribe runs whisper on a span of it."},
	"task.worker":    {Label: "Worker", Help: "The worker that holds or finished the task. Workers you may not see are not named."},
	"task.leaseage":  {Label: "Lease age", Help: "How long the worker has held the task."},
	"task.heartbeat": {Label: "Last heartbeat", Help: "When the worker last reported progress on the task; it renews the lease."},
	"task.attempt":   {Label: "Attempt", Help: "Which attempt this is, counting workers that failed the task or let it expire."},
	"task.audio":     {Label: "Audio", Help: "The span of the job's audio the task transcribed."},
	"task.engine":    {Label: "Engine", Help: "The whisper.cpp (transcribe) or ffmpeg (prepare) version that ran the task."},
	"task.compute":   {Label: "Compute", Help: "Wall time the worker spent on the task."},
	"task.state":     {Label: "State", Help: "pending: waiting for a worker; failed: failed for good; cancelled: will not run because the job failed."},
	"task.waiting":   {Label: "Waiting", Help: "How long a pending task has waited since it last became pending."},
	"task.attempts":  {Label: "Attempts", Help: "Workers that failed the task or let it expire so far."},
	"task.lastfail":  {Label: "Last failure", Help: "The latest failure reason and the worker it happened on."},
	"task.why": {Label: "Why waiting", Help: "Why no eligible worker can take the task right now, best effort. Empty: it is waiting its turn.",
		More: designDoc + "#63-ui-ui"},

	// Bulk results.
	"bulk.job":     {Label: "Job", Help: "The job acted on."},
	"bulk.outcome": {Label: "Outcome", Help: "What happened to the job, and why if it was skipped."},

	// Workers table.
	"workers.worker": {Label: "Worker", Help: "The worker's description, else its hostname, with the start of its id; opens the worker."},
	"workers.state":  {Label: "State", Help: "pending: awaiting approval; active: taking work; paused: finishing current work, taking none; rejected: token revoked."},
	"workers.queues": {Label: "Queues", Help: "Queues the worker may process (its process grants)."},
	"workers.sandbox": {Label: "Sandbox", Help: "How strongly the worker can isolate media tools. Converting uploads (prepare tasks) needs the server's minimum, full by default.",
		More: designDoc + "#74-priority-and-sandbox"},
	"workers.version": {Label: "Version", Help: "The worker's whisper-network version compared with this server's: outdated is older, a note marks newer, differs when they cannot be ordered. Hover a version for the engine and ffmpeg versions."},
	"workers.slots":   {Label: "Slots", Help: "Busy / total task slots, from the last heartbeat; – before one."},
	"workers.seen":    {Label: "Last seen", Help: "When the worker last contacted the server; online if active and seen in the last few minutes."},
	"workers.done":    {Label: "Tasks done", Help: "Tasks the worker has completed; hover for the split by kind."},
	"workers.audio":   {Label: "Audio hours", Help: "Hours of audio the worker has transcribed."},
	"workers.rtf":     {Label: "Avg RTF", Help: "Average realtime factor: seconds of audio transcribed per second of compute. Higher is faster."},
	"workers.failure": {Label: "Failure rate", Help: "Failed or expired leases as a share of all the worker's counted task outcomes."},
	"workers.actions": {Label: "Actions", Help: "Controls for the worker's state, where you may change it."},

	// Clients.
	"clients.name":    {Label: "Name", Help: "The client's name, chosen when it was created; opens the client."},
	"clients.id":      {Label: "ID", Help: "The client's id, which audit entries and job owners refer to."},
	"clients.roles":   {Label: "Roles", Help: "The client's grants: a role on a scope (* or a queue).", More: designDoc + "#5-authorization"},
	"clients.created": {Label: "Created", Help: "When the client was created."},
	"clients.used":    {Label: "Last used", Help: "When the client's token was last used."},
	"clients.actions": {Label: "Actions", Help: "Edit permissions or revoke the token, where you may."},
	"activity.time":   {Label: "Time", Help: "When it happened (UTC)."},
	"activity.action": {Label: "Action", Help: "What the client did, as the audit log records it."},
	"activity.target": {Label: "Target", Help: "What it acted on."},

	// Users.
	"users.user":    {Label: "User", Help: "The user's sign-in name."},
	"users.status":  {Label: "Status", Help: "active; must change password at next sign-in; or disabled."},
	"users.roles":   {Label: "Roles", Help: "The user's grants: a role on a scope (*, a queue or a worker).", More: designDoc + "#5-authorization"},
	"users.created": {Label: "Created", Help: "When the user was created."},
	"users.actions": {Label: "Actions", Help: "Disable the user, ending their sessions."},

	// Grant matrix.
	"matrix.queue": {Label: "Queue", Help: "Roles in this row apply to this queue only."},

	// Audit log.
	"audit.time":   {Label: "Time", Help: "When it happened (UTC)."},
	"audit.actor":  {Label: "Actor", Help: "Who did it: a user, an API client, a worker, the server itself (system) or the offline admin command."},
	"audit.action": {Label: "Action", Help: "What was done, as <area>.<verb>, e.g. worker.approve."},
	"audit.target": {Label: "Target", Help: "What it was done to: a user, client, worker, job or queue."},
	"audit.detail": {Label: "Detail", Help: "The change's parameters, as JSON."},
}

// col returns the header of column key, with an optional cell class. A
// page with the same column in two tables tells them apart with a suffix,
// "key@table", which only changes the help's DOM id. An unknown key is an
// error, which fails the page render.
func col(key string, class ...string) (column, error) {
	name, _, _ := strings.Cut(key, "@")
	c, ok := columns[name]
	if !ok {
		return column{}, fmt.Errorf("col: no help for column %q", name)
	}
	c.ID = "help-" + strings.NewReplacer(".", "-", "@", "-").Replace(key)
	c.Class = strings.Join(class, " ")
	return c, nil
}

// colText is a header whose label and help are data (the grant matrix's
// role columns); key makes its DOM id unique.
func colText(key, label, help string, class ...string) column {
	return column{ID: "help-" + key, Label: label, Help: help, Class: strings.Join(class, " ")}
}

// valueHelp explains badge values where they appear (#79), keyed by
// "kind:value".
var valueHelp = map[string]string{
	"job:preparing":    "Converting the upload to canonical audio.",
	"job:queued":       "Waiting for a worker.",
	"job:running":      "Tasks are in progress.",
	"job:done":         "The transcript is ready.",
	"job:failed":       "Failed for good; it can be retried while its media is kept.",
	"task:pending":     "Waiting for a worker.",
	"task:failed":      "Failed for good.",
	"task:cancelled":   "Will not run: the job failed.",
	"worker:pending":   "Registered, awaiting approval by an admin.",
	"worker:active":    "Approved and taking work.",
	"worker:paused":    "Finishing current work, taking no new tasks.",
	"worker:rejected":  "Refused by an administrator; its token is revoked.",
	"badge:auto":       "Paused automatically by the circuit breaker after repeated failures.",
	"badge:busyhost":   "The host is busy with other work, so the worker is not taking tasks.",
	"badge:outdated":   "Older than the server's version.",
	"badge:differs":    "Cannot be ordered against the server's version.",
	"badge:online":     "Active and seen in the last few minutes.",
	"badge:revoked":    "The token is revoked; programs using it are refused.",
	"badge:noeligible": "No active worker may process this queue: pending tasks cannot run.",
}

// vhelp returns the help for a badge value, or "".
func vhelp(kind string, value any) string { return valueHelp[kind+":"+fmt.Sprint(value)] }
