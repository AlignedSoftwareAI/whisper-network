package store

import (
	"fmt"
	"strconv"
	"strings"
)

// Principal and scope string forms used in grants (DESIGN.md §5).
const (
	ScopeAll = "*"
	// SystemActor is the granted_by/actor recorded for bootstrap actions.
	SystemActor = "system"
)

// UserPrincipal returns "user:<id>".
func UserPrincipal(id int64) string { return "user:" + strconv.FormatInt(id, 10) }

// ClientPrincipal returns "client:<id>".
func ClientPrincipal(id string) string { return "client:" + id }

// WorkerPrincipal returns "worker:<id>".
func WorkerPrincipal(id string) string { return "worker:" + id }

// QueueScope returns "queue:<id>".
func QueueScope(id int64) string { return "queue:" + strconv.FormatInt(id, 10) }

// WorkerScope returns "worker:<id>".
func WorkerScope(id string) string { return "worker:" + id }

// ParseQueueScope extracts the queue id from a "queue:<id>" scope.
func ParseQueueScope(scope string) (int64, error) {
	rest, ok := strings.CutPrefix(scope, "queue:")
	if !ok {
		return 0, fmt.Errorf("%w: scope %q is not a queue scope", ErrInvalid, scope)
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: scope %q: %w", ErrInvalid, scope, err)
	}
	return id, nil
}
