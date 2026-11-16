package ui

import (
	"context"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/store"
)

// Workers, jobs and API clients are shown by name, with a short id for
// precision, linked to their page (#57, #58, #60; DESIGN.md §6.3). Every
// page builds its refs with a labeler and renders them with the "ref"
// template, so the format cannot drift between pages. What a ref reveals
// follows the visibility rules: a worker's identity only to those who may
// see it (seesWorker, or mayViewWorker outside a job), a job's label only to
// those who may see the job's content, a client's name only to client
// admins.

// ref is a named entity for display.
type ref struct {
	Name  string // the name; empty shows Short alone
	Short string // abbreviated id, after the name in parentheses
	Title string // tooltip: the full id (and the full name when truncated)
	Href  string // link target; empty: unlinked
	Note  string // a state shown after it: removed, revoked, deleted
	// Muted marks generic text standing in for something hidden ("a
	// client"), with no id or link.
	Muted bool
}

// maxLabel is the longest job label shown whole; longer ones are cut with
// an ellipsis, the full label in the tooltip.
const maxLabel = 60

// labelIDs collects the ids a page will name, so each kind is looked up
// once.
type labelIDs struct{ workers, jobs, clients []string }

func (l *labelIDs) worker(ids ...string) { l.workers = append(l.workers, ids...) }
func (l *labelIDs) job(ids ...string)    { l.jobs = append(l.jobs, ids...) }
func (l *labelIDs) client(ids ...string) { l.clients = append(l.clients, ids...) }

// principal collects the id in a principal or target string
// ("worker:<id>", "client:<id>", "job:<id>").
func (l *labelIDs) principal(p string) {
	kind, id, _ := strings.Cut(p, ":")
	switch kind {
	case "worker":
		l.worker(id)
	case "client":
		l.client(id)
	case "job":
		l.job(id)
	}
}

// labeler names the entities of one page for one principal.
type labeler struct {
	rc      *reqCtx
	workers map[string]store.WorkerRef
	jobs    map[string]store.JobRef
	clients map[string]store.ClientRef
}

// labels resolves ids: one query per kind that has any.
func (u *UI) labels(ctx context.Context, rc *reqCtx, ids labelIDs) (*labeler, error) {
	st := u.srv.Store()
	l := &labeler{rc: rc}
	var err error
	if l.workers, err = st.WorkerRefs(ctx, ids.workers); err != nil {
		return nil, err
	}
	if l.jobs, err = st.JobRefs(ctx, ids.jobs); err != nil {
		return nil, err
	}
	if l.clients, err = st.ClientRefs(ctx, ids.clients); err != nil {
		return nil, err
	}
	return l, nil
}

// worker names a worker. On a job page queue is the job's queue and the
// worker is shown only to those who may see who processes it
// (seesWorker); elsewhere queue is "" and the rule is mayViewWorker. A
// worker that is gone (pruned) shows its id alone, unlinked.
func (l *labeler) worker(id, queue string) ref {
	visible := mayViewWorker(l.rc, id)
	if queue != "" {
		visible = l.rc.seesWorker(queue, store.WorkerRef{ID: id})
	}
	if !visible {
		return ref{Name: "a worker", Muted: true}
	}
	w, ok := l.workers[id]
	if !ok {
		return ref{Short: shortID(id), Title: id, Note: "removed"}
	}
	return workerRef(w)
}

// workerRef is the ref of a worker the viewer may see.
func workerRef(w store.WorkerRef) ref {
	r := ref{Short: shortID(w.ID), Title: w.ID, Href: workerPath(w.ID)}
	if name := refName(w); name != shortID(w.ID) {
		r.Name = name
	}
	return r
}

// job names a job by its label, else its source, else its short id. A job
// the viewer may not see shows its short id only, unlinked (no label: it is
// client text); a deleted one is marked.
func (l *labeler) job(id string) ref {
	short := jobShort(id)
	j, ok := l.jobs[id]
	if !ok {
		return ref{Short: short, Title: id, Note: "deleted"}
	}
	res := auth.Resource{Queue: j.QueueName, OwnerID: j.ClientID}
	if !l.rc.can(auth.ActViewJob, res) {
		return ref{Short: short, Title: id}
	}
	r := ref{Short: short, Title: id, Href: "/ui/jobs/" + url.PathEscape(id)}
	if l.rc.can(auth.ActViewJobContent, res) {
		r.Name, r.Title = jobName(j.Label, j.Source, id)
	}
	return r
}

// jobName is the displayed name of a job and its tooltip: the label (else
// the source), cut to maxLabel runes.
func jobName(label, source, id string) (name, title string) {
	name = strings.TrimSpace(label)
	if name == "" {
		name = strings.TrimSpace(source)
	}
	title = id
	if utf8.RuneCountInString(name) > maxLabel {
		title = name + " · " + id
		name = string([]rune(name)[:maxLabel-1]) + "…"
	}
	return name, title
}

// jobShort abbreviates a job id (a UUIDv7, whose start is a timestamp
// shared by jobs created together) to its last four characters.
func jobShort(id string) string {
	if len(id) <= 8 {
		return id
	}
	return "…" + id[len(id)-4:]
}

// client names an API client for client admins; anyone else sees "a
// client". A revoked client keeps its name, marked.
func (l *labeler) client(id string) ref {
	if !l.rc.can(auth.ActViewClient, auth.Resource{ClientID: id}) {
		return ref{Name: "a client", Muted: true}
	}
	c, ok := l.clients[id]
	if !ok {
		return ref{Short: clientShort(id), Title: id, Note: "removed"}
	}
	r := ref{Name: c.Name, Short: clientShort(id), Title: id, Href: "/ui/clients/" + url.PathEscape(id)}
	if c.Revoked {
		r.Note = "revoked"
	}
	return r
}

// clientShort abbreviates a client id: its prefix and four characters.
func clientShort(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:8] + "…"
}

// principal names a principal or audit target: workers, clients and jobs
// as above, users by name, queues linked; anything else (the system, the
// admin command) as written.
func (l *labeler) principal(p string) ref {
	kind, id, ok := strings.Cut(p, ":")
	if !ok {
		return ref{Name: p}
	}
	switch kind {
	case "worker":
		return l.worker(id, "")
	case "client":
		return l.client(id)
	case "job":
		return l.job(id)
	case "user":
		return ref{Name: id, Note: "user"}
	case "queue":
		return ref{Name: id, Note: "queue", Href: "/ui/queues/" + url.PathEscape(id)}
	}
	return ref{Name: p}
}

// segment is a piece of text that may name a worker.
type segment struct {
	Text string
	Ref  *ref
}

// withWorkers splits text (a failure reason) around the ids of workers,
// naming each as worker(id, queue) does, so a reason never shows a worker
// identity its reader may not see.
func (l *labeler) withWorkers(text, queue string, workers []string) []segment {
	segs := []segment{{Text: text}}
	for _, id := range workers {
		if id == "" {
			continue
		}
		r := l.worker(id, queue)
		var out []segment
		for _, s := range segs {
			if s.Ref != nil {
				out = append(out, s)
				continue
			}
			parts := strings.Split(s.Text, id)
			for i, p := range parts {
				if i > 0 {
					out = append(out, segment{Ref: &r})
				}
				if p != "" {
					out = append(out, segment{Text: p})
				}
			}
		}
		segs = out
	}
	return segs
}
