package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
)

// bulkView is the bulk action page: first a confirmation, then the result.
type bulkView struct {
	Queue  string
	Action string // api.BulkRetry or api.BulkDelete
	// JobIDs are the selected jobs; empty when the selection is the filter.
	JobIDs []string
	// State is the filter of an all-matching selection.
	State string
	Count int // jobs selected (for the filter: matching now)
	Back  string

	Done    bool
	Summary string
	Results []api.BulkJobResult
	Refs    map[string]ref // each result's job, named (#58)
}

// maxBulkPages bounds the pages one confirmed all-matching action walks
// (each at most api.MaxBulkJobs jobs).
const maxBulkPages = 100

// handleBulk is the queue page's bulk retry or delete (#68). The form posts
// op (retry-selected, delete-selected, retry-all, delete-all) with the
// checked job_id values or the state filter; without confirm=1 it shows a
// confirmation with the count, with it the server's per-job outcomes and a
// summary. Authorization is per job, in server.BulkJobs, exactly as POST
// /queue/{q}/bulk.
func (u *UI) handleBulk(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	ctx := r.Context()
	q, err := u.srv.Store().QueueByName(ctx, r.PathValue("q"))
	if err != nil {
		return err
	}
	if auth.VisibilityOn(rc.P.Principal, q.Name) == auth.SeesNone {
		return u.deny(r, rc, "no access to queue "+q.Name)
	}
	if err := r.ParseForm(); err != nil {
		return badRequest("Malformed form.")
	}
	action, scope, _ := strings.Cut(r.PostFormValue("op"), "-")
	if action != api.BulkRetry && action != api.BulkDelete || scope != "selected" && scope != "all" {
		return badRequest("Unknown bulk action.")
	}
	v := bulkView{Queue: q.Name, Action: action, Back: "/ui/queues/" + url.PathEscape(q.Name)}
	req := api.BulkRequest{Action: action}
	if scope == "selected" {
		v.JobIDs = r.PostForm["job_id"]
		if len(v.JobIDs) == 0 {
			return badRequest("Select at least one job.")
		}
		if len(v.JobIDs) > api.MaxBulkJobs {
			return badRequest(fmt.Sprintf("Select at most %d jobs, or use select all matching.", api.MaxBulkJobs))
		}
		v.Count = len(v.JobIDs)
		req.JobIDs = v.JobIDs
	} else {
		v.State = r.PostFormValue("state")
		if !validJobState(api.JobState(v.State)) {
			return badRequest("Select all matching needs a state filter.")
		}
		req.Filter = &api.BulkFilter{State: api.JobState(v.State)}
	}
	if r.PostFormValue("confirm") != "1" {
		if req.Filter != nil {
			// The count the operator confirms is what the action would
			// touch: the dry run walks the same pages.
			req.DryRun = true
			res, err := u.bulkAll(r, rc, q.Name, req)
			if err != nil {
				return err
			}
			v.Count = len(res.Jobs)
		}
		u.render(w, r, rc, http.StatusOK, "bulk", "Confirm "+action, v)
		return nil
	}
	res, err := u.bulkAll(r, rc, q.Name, req)
	if err != nil {
		return err
	}
	v.Done, v.Results, v.Summary = true, res.Jobs, bulkSummary(res.Jobs)
	var ids labelIDs
	for _, j := range res.Jobs {
		ids.job(j.JobID)
	}
	lb, err := u.labels(r.Context(), rc, ids)
	if err != nil {
		return err
	}
	v.Refs = make(map[string]ref, len(res.Jobs))
	for _, j := range res.Jobs {
		v.Refs[j.JobID] = lb.job(j.JobID)
	}
	u.render(w, r, rc, http.StatusOK, "bulk", "Bulk "+action, v)
	return nil
}

// bulkAll runs a bulk request, following the cursor of a filter selection
// through every page (at most maxBulkPages), and merges the outcomes.
func (u *UI) bulkAll(r *http.Request, rc *reqCtx, queue string, req api.BulkRequest) (*api.BulkResult, error) {
	all := &api.BulkResult{Action: req.Action, DryRun: req.DryRun, Counts: map[string]int{}}
	for range maxBulkPages {
		res, err := u.srv.BulkJobs(r.Context(), rc.P, queue, req)
		if err != nil {
			return nil, err
		}
		all.Jobs = append(all.Jobs, res.Jobs...)
		for k, n := range res.Counts {
			all.Counts[k] += n
		}
		if res.NextCursor == "" || req.Filter == nil {
			break
		}
		f := *req.Filter
		f.Cursor = res.NextCursor
		req.Filter = &f
	}
	return all, nil
}

// bulkSummary is a one-line tally: "3 retried, 1 skipped: media purged".
func bulkSummary(rs []api.BulkJobResult) string {
	counts := map[string]int{}
	var keys []string
	for _, r := range rs {
		k := r.Outcome
		if r.Reason != "" {
			k += ": " + r.Reason
		}
		if counts[k] == 0 {
			keys = append(keys, k)
		}
		counts[k]++
	}
	rank := func(k string) int {
		switch {
		case k == api.OutcomeRetried || k == api.OutcomeDeleted:
			return 0
		case strings.HasPrefix(k, api.OutcomeSkipped):
			return 1
		}
		return 2
	}
	sort.SliceStable(keys, func(i, j int) bool { return rank(keys[i]) < rank(keys[j]) })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", counts[k], strings.ReplaceAll(k, "_", " "))
	}
	if len(parts) == 0 {
		return "No jobs matched."
	}
	return strings.Join(parts, ", ")
}
