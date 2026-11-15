package server

import (
	"net/http"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
)

// handlePatchQueue is PATCH /queue/{q}. Weight is a fleet-wide decision
// (admin only); description and upload cap belong to the queue admin.
func (s *Server) handlePatchQueue(w http.ResponseWriter, r *http.Request, p *Principal) error {
	q, err := s.st.QueueByName(r.Context(), r.PathValue("q"))
	if err != nil {
		return err
	}
	var patch api.QueuePatch
	if err := decodeJSON(w, r, &patch, false); err != nil {
		return err
	}
	if patch == (api.QueuePatch{}) {
		return badRequest("nothing to change")
	}
	res := auth.Resource{Queue: q.Name}
	checks := []struct {
		set bool
		act auth.Action
		why string
	}{
		{patch.Weight != nil, auth.ActSetQueueWeight, "only admin may change a queue's weight"},
		{patch.Description != nil, auth.ActPatchQueueDescription, "queue_admin required"},
		{patch.MaxUploadBytes != nil, auth.ActPatchQueueMaxUpload, "queue_admin required"},
	}
	for _, c := range checks {
		if c.set && !auth.Can(p.Principal, c.act, res) {
			return s.deny(r, p, c.why)
		}
	}
	out, err := s.ops.PatchQueue(r.Context(), p.Actor(), q, patch)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out.API())
	return nil
}

// handleCreateClient is POST /client: a client_admin creates a client with
// grants it may hand out (auth.CanDelegateAtCreation: a subset of its own,
// or what it could grant as an administrator).
func (s *Server) handleCreateClient(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if !auth.Can(p.Principal, auth.ActCreateClient, auth.Resource{}) {
		return s.deny(r, p, "client_admin required")
	}
	var req api.CreateClientRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	if err := auth.CanDelegateAtCreation(p.Principal, req.Grants); err != nil {
		return s.deny(r, p, err.Error())
	}
	c, err := s.ops.CreateClient(r.Context(), p.Actor(), req.Name, req.Grants)
	if err != nil {
		return err
	}
	w.Header().Set("Location", s.publicURL("/client/"+c.ID))
	writeJSON(w, http.StatusCreated, c)
	return nil
}

func (s *Server) handleGetClient(w http.ResponseWriter, r *http.Request, p *Principal) error {
	id := r.PathValue("id")
	if !auth.Can(p.Principal, auth.ActViewClient, auth.Resource{ClientID: id}) {
		return s.deny(r, p, "client_admin required")
	}
	c, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		return err
	}
	out, err := s.ops.ClientAPI(r.Context(), c)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleRevokeClient is DELETE /client/{id}: client_admin, and only for a
// client the actor could have created (auth.CanRevokeClient).
func (s *Server) handleRevokeClient(w http.ResponseWriter, r *http.Request, p *Principal) error {
	id := r.PathValue("id")
	if !auth.Can(p.Principal, auth.ActRevokeClient, auth.Resource{ClientID: id}) {
		return s.deny(r, p, "client_admin required")
	}
	c, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		return err
	}
	held, err := s.ops.WireGrants(r.Context(), c.Principal())
	if err != nil {
		return err
	}
	if err := auth.CanRevokeClient(p.Principal, id, held); err != nil {
		return s.deny(r, p, err.Error())
	}
	if err := s.ops.RevokeClient(r.Context(), p.Actor(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
