package api

import "time"

// Admin API wire types (server routes under /admin/, see internal/server
// admin.go). Principals on the wire are "user:<username>",
// "client:<id>" or "worker:<id>"; scopes are "*", "queue:<name>" or
// "worker:<id>".

// WorkerSummary is one entry of GET /admin/workers: the worker document
// plus the approval record and latest heartbeat gauges.
type WorkerSummary struct {
	Worker
	RegisteredAt time.Time  `json:"registered_at"`
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	Gauges       *Gauges    `json:"gauges,omitempty"`
}

// ApproveWorkerRequest is the body of POST /admin/workers/{id}/approve:
// make the worker active and grant it process on each queue.
type ApproveWorkerRequest struct {
	Queues []string `json:"queues"`
}

// CreateUserRequest is the body of POST /admin/users. An empty Password
// makes the server generate one, returned once in User.Password.
type CreateUserRequest struct {
	Username string  `json:"username"`
	Password string  `json:"password,omitempty"`
	Grants   []Grant `json:"grants,omitempty"`
}

// User is a UI login. Password is set only in the response that generated it.
type User struct {
	Username           string    `json:"username"`
	MustChangePassword bool      `json:"must_change_password"`
	Disabled           bool      `json:"disabled"`
	CreatedAt          time.Time `json:"created_at"`
	Grants             []Grant   `json:"grants"`
	Password           string    `json:"password,omitempty"`
}

// PrincipalGrant is a grant bound to a principal, for /admin/grants.
type PrincipalGrant struct {
	Principal string `json:"principal"`
	Role      Role   `json:"role"`
	Scope     string `json:"scope"`
	GrantedBy string `json:"granted_by,omitempty"`
}

// AuditEntry is one audit record.
type AuditEntry struct {
	ID     int64     `json:"id"`
	TS     time.Time `json:"ts"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// AuditList is GET /admin/audit. Pass NextBefore as ?before= for the next
// (older) page; 0 means there is none.
type AuditList struct {
	Entries    []AuditEntry `json:"entries"`
	NextBefore int64        `json:"next_before,omitempty"`
}
