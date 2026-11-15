package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

const testToken = "wnc_testtokentesttoken"

// newTestClient serves mux on an httptest server and returns a client for it.
func newTestClient(t *testing.T, mux http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, WithToken(testToken), WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	return c, srv
}

// problem writes an RFC 9457 body the way the server does.
func problem(w http.ResponseWriter, status int, typ, state string) {
	w.Header().Set("Content-Type", api.ProblemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Problem{Type: typ, Title: http.StatusText(status), Status: status, State: state})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// requireAuth fails the request with 401 unless the test token is present.
func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			problem(w, http.StatusUnauthorized, api.ProblemUnauthorized, "")
			return
		}
		h(w, r)
	}
}
