package obs

import (
	"io"
	"net/http"

	"github.com/VictoriaMetrics/metrics"
)

// WritePrometheus writes the catalog plus the library's go_* and process_*
// metrics to w in Prometheus text exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	m.set.WritePrometheus(w)
	metrics.WriteProcessMetrics(w)
}

// Handler serves GET /metrics.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		m.WritePrometheus(w)
	})
}
