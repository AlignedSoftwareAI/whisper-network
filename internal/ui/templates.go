package ui

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/deweysasser/whisper-network/internal/media"
)

//go:embed templates
var templateFS embed.FS

// parsePages parses the layout once and, for each page file, a clone of it
// with that page's templates added. Pages define "content" (and may define
// fragments that handlers render alone). A parse error is a build defect
// and panics; every test that builds a server catches it.
func parsePages(funcs template.FuncMap) map[string]*template.Template {
	base := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html"))
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	pages := map[string]*template.Template{}
	for _, f := range files {
		name := strings.TrimSuffix(path.Base(f), ".html")
		if name == "layout" {
			continue
		}
		pages[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFS, f))
	}
	return pages
}

// funcs are the template helpers. None returns template.HTML: everything a
// template prints is escaped by html/template.
func (u *UI) funcs() template.FuncMap {
	return template.FuncMap{
		"when":  formatTime,
		"whenp": formatTimePtr,
		"ago":   func(t *time.Time) string { return ago(t, u.srv.Now()) },
		"dur":   formatDuration,
		"secs":  func(s float64) string { return formatDuration(time.Duration(s * float64(time.Second))) },
		"secsp": formatSecsPtr,
		"bytes": formatBytes,
		"pct":   func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
		"num":   func(f float64) string { return fmt.Sprintf("%.1f", f) },
		"hours": formatHours,
		"join":  strings.Join,
		"short": shortID,
		"dict":  dict,
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return formatTime(*t)
}

func formatSecsPtr(s *float64) string {
	if s == nil {
		return "—"
	}
	return formatDuration(time.Duration(*s * float64(time.Second)))
}

// ago renders how long before now t was, coarsely.
func ago(t *time.Time, now time.Time) string {
	if t == nil {
		return "never"
	}
	d := now.Sub(*t)
	if d < time.Second {
		return "just now"
	}
	return formatDuration(d) + " ago"
}

// formatDuration renders a duration with at most two units.
func formatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

// formatHours renders an hour count with two decimals below 10 (small
// fleets) and one above.
func formatHours(h float64) string {
	if h < 10 {
		return fmt.Sprintf("%.2f", h)
	}
	return fmt.Sprintf("%.1f", h)
}

// formatBytes renders a size with binary units.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// shortID abbreviates a long identifier for display; the full value stays
// in links and titles.
func shortID(s string) string {
	if len(s) <= 13 {
		return s
	}
	return s[:8] + "…" + s[len(s)-4:]
}

// dict builds a map from alternating keys and values, for passing several
// values to a sub-template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// samplesToSeconds converts canonical-audio samples to seconds.
func samplesToSeconds(n int64) float64 { return float64(n) / media.SampleRate }
