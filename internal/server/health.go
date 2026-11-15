package server

import (
	"fmt"
	"net/http"

	"github.com/deweysasser/whisper-network/internal/api"
)

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz reports ready when the database accepts a write transaction
// and the data directory has at least MinFreeBytes free.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.st.Ping(r.Context()); err != nil {
		s.log.Warn().Err(err).Msg("readiness: database not writable")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("database not writable\n"))
		return
	}
	if err := s.checkFreeSpace(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintln(w, err.(*problem).detail)
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

// checkFreeSpace returns a 507 problem when the data directory is below the
// free-space threshold. A failing probe is treated as full: refusing work is
// safer than filling the disk.
func (s *Server) checkFreeSpace() error {
	free, err := s.freeBytes(s.cfg.DataDir)
	if err != nil {
		s.log.Warn().Err(err).Msg("free space probe failed")
		return newProblem(http.StatusInsufficientStorage, api.ProblemStorageFull, "cannot determine free space")
	}
	if free < s.cfg.MinFreeBytes {
		return newProblem(http.StatusInsufficientStorage, api.ProblemStorageFull,
			fmt.Sprintf("only %d bytes free in the data directory, need %d", free, s.cfg.MinFreeBytes))
	}
	return nil
}
