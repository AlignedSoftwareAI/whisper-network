// Package obs holds the observability vocabulary shared by the server and the
// worker (DESIGN.md §10): stable log event and field names, a mode-scoped
// logger, the never-log rules, and the Prometheus/VictoriaMetrics metric
// catalog.
//
// Logging uses the program's zerolog logger. This package does not configure
// it; it only fixes the names that dashboards and alerts key on, so a single
// query can follow one job from the server's lease event to the worker's
// result. Metrics use github.com/VictoriaMetrics/metrics through an injected
// *metrics.Set; nothing here touches the library's default set.
package obs
