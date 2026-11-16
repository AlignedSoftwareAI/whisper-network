package server

import (
	"bytes"
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
)

// llmsTemplate is /llms.txt (https://llmstxt.org): a Markdown guide that
// tells an LLM agent how to use this server's client API. It is rendered
// once per server from instance settings that an anonymous caller may know
// (models, limits, retention, the configured public URL) and never from
// anything that needs authorization, such as queue names or workers.
//
//go:embed llms.txt.tmpl
var llmsTemplate string

// llmsData are the instance values substituted into llms.txt.tmpl.
type llmsData struct {
	BaseURL           string // the configured public URL, or "" when unset
	BaseOrPlaceholder string // BaseURL, or a placeholder for examples
	Models            []string
	ExampleModel      string
	MaxUpload         string
	MaxPrompt         int
	MaxLabel          int
	MaxMetadata       int
	AudioRetention    string
	FailedRetention   string
	JobRetention      string
}

// renderLLMs renders llms.txt for cfg. The base URL comes only from the
// configured PublicURL, never from the request's Host header, so the page
// cannot be made to advertise an attacker-chosen host.
func renderLLMs(cfg Config) ([]byte, error) {
	t, err := template.New("llms.txt").Parse(llmsTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse llms.txt template: %w", err)
	}
	base := strings.TrimRight(cfg.PublicURL, "/")
	d := llmsData{
		BaseURL:           base,
		BaseOrPlaceholder: base,
		Models:            cfg.Models,
		MaxUpload:         humanBytes(cfg.MaxUploadBytes),
		MaxPrompt:         api.MaxPromptLen,
		MaxLabel:          api.MaxLabelLen,
		MaxMetadata:       api.MaxMetadataBytes,
		AudioRetention:    humanDuration(cfg.AudioRetention),
		FailedRetention:   humanDuration(cfg.FailedRetention),
		JobRetention:      humanDuration(cfg.JobRetention),
	}
	if d.BaseOrPlaceholder == "" {
		d.BaseOrPlaceholder = "https://SERVER"
	}
	if len(cfg.Models) > 0 {
		d.ExampleModel = cfg.Models[0]
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return nil, fmt.Errorf("render llms.txt: %w", err)
	}
	return b.Bytes(), nil
}

// handleLLMs serves the pre-rendered llms.txt. It is public: everything in
// it is either documentation or a setting a client discovers anyway on its
// first upload.
func (s *Server) handleLLMs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(s.llms)
}

// humanBytes formats n in binary units, e.g. 2 GiB or 512 MiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v, i := float64(n)/unit, 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d %s", int64(v), units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// humanDuration formats d as whole days or hours, e.g. "30 days", "24 hours".
func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	case d >= time.Hour && d%time.Hour == 0:
		if d == time.Hour {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", d/time.Hour)
	default:
		return d.String()
	}
}
