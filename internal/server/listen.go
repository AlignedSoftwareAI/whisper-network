package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/deweysasser/whisper-network/internal/obs"
)

// DBPath returns the SQLite database path inside a data directory.
func DBPath(dataDir string) string { return filepath.Join(dataDir, "whisper-network.db") }

// BlobsPath returns the blob directory inside a data directory.
func BlobsPath(dataDir string) string { return filepath.Join(dataDir, "blobs") }

// ListenMetrics serves GET /metrics on its own listener (DESIGN.md §10.2:
// never on the public port) and returns a function that shuts it down.
func ListenMetrics(ctx context.Context, addr string, m *obs.Metrics, lg zerolog.Logger) (stop func(), err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Error().Err(err).Msg("metrics listener failed")
		}
	}()
	return func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}, nil
}

// IsLoopbackAddr reports whether a listen address binds only to loopback,
// so serving plain HTTP on it does not expose tokens to the network.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
