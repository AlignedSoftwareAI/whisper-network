package ui

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// auditPageSize is the number of audit entries per page.
const auditPageSize = 50

// auditView is one page of the audit log, newest first, with the filter
// that selected it (echoed into the filter form).
type auditView struct {
	Entries  []auditRow
	Filter   auditFilterForm
	NextPage string
}

// auditRow is an entry with its actor and target named (#57, #58, #60).
type auditRow struct {
	store.AuditEntry
	ActorRef, TargetRef ref
}

// auditFilterForm holds the filter fields as typed.
type auditFilterForm struct {
	Action, Actor, Target, Since, Until string
}

// handleAudit pages through the audit log (admin, as GET /admin/audit),
// filtered by action prefix, actor, target and time range with the same
// parameters as the API.
func (u *UI) handleAudit(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	if !rc.can(auth.ActViewAudit, auth.Resource{}) {
		return u.deny(r, rc, "admin required")
	}
	q := r.URL.Query()
	q.Del("limit")
	f, err := server.ParseAuditFilter(q)
	if err != nil {
		_, detail := server.ErrorStatus(err)
		return badRequest(detail)
	}
	f.Limit = auditPageSize
	es, err := u.srv.Store().QueryAudit(r.Context(), f)
	if err != nil {
		return err
	}
	var ids labelIDs
	for _, e := range es {
		ids.principal(e.Actor)
		ids.principal(e.Target)
	}
	lb, err := u.labels(r.Context(), rc, ids)
	if err != nil {
		return err
	}
	rows := make([]auditRow, len(es))
	for i, e := range es {
		rows[i] = auditRow{AuditEntry: e, ActorRef: lb.principal(e.Actor)}
		if e.Target != "" {
			rows[i].TargetRef = lb.principal(e.Target)
		}
	}
	v := auditView{Entries: rows, Filter: auditFilterForm{Action: q.Get("action"), Actor: q.Get("actor"),
		Target: q.Get("target"), Since: q.Get("since"), Until: q.Get("until")}}
	if len(es) == auditPageSize {
		next := url.Values{}
		for _, k := range []string{"action", "actor", "target", "since", "until"} {
			if s := q.Get(k); s != "" {
				next.Set(k, s)
			}
		}
		next.Set("before", strconv.FormatInt(es[len(es)-1].ID, 10))
		v.NextPage = "/ui/audit?" + next.Encode()
	}
	u.render(w, r, rc, http.StatusOK, "audit", "Audit log", v)
	return nil
}
