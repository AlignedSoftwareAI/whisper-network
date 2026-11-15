package obs

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/rs/zerolog"
)

// PushConfig configures periodic push to a VictoriaMetrics-compatible
// /api/v1/import/prometheus endpoint.
type PushConfig struct {
	// URL is the import endpoint, for example
	// http://vm:8428/api/v1/import/prometheus.
	URL string
	// Interval between pushes; DESIGN.md §10.2 defaults to 15s.
	Interval time.Duration
	// ExtraLabels are added to every pushed series (--metrics-labels).
	ExtraLabels map[string]string
	// Headers are optional extra request headers in "Name: value" form.
	Headers []string
	// Logger receives push errors; the zero logger discards them.
	Logger zerolog.Logger
}

// StartPush pushes m's metrics, including go_* and process_*, until ctx is
// cancelled. It validates the configuration synchronously and then pushes in
// the background. The returned channel is closed once the push loop has
// stopped.
func (m *Metrics) StartPush(ctx context.Context, cfg PushConfig) (<-chan struct{}, error) {
	var wg sync.WaitGroup
	log := cfg.Logger
	opts := &metrics.PushOptions{
		ExtraLabels: FormatLabels(cfg.ExtraLabels),
		Headers:     cfg.Headers,
		Method:      http.MethodPost,
		WaitGroup:   &wg,
		LogErr: func(format string, v ...any) {
			log.Warn().Msgf(format, v...)
		},
	}
	err := metrics.InitPushExtWithOptions(ctx, cfg.URL, cfg.Interval, m.WritePrometheus, opts)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	return done, nil
}
