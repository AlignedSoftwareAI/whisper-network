package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"path"
	"time"
)

// The vendored htmx release; see the package documentation.
const (
	htmxVersion = "2.0.4"
	htmxSHA256  = "e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447"
)

//go:embed static
var staticFS embed.FS

// asset is one embedded static file, served with a long-lived cache header.
// Pages reference it with a ?v=<hash> suffix so a new build busts caches.
type asset struct {
	body  []byte
	ctype string
	hash  string // hex sha256 of body
}

// contentType returns the served type of an embedded file, chosen
// explicitly so the response never depends on the platform's MIME table.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	}
	return "application/octet-stream"
}

// loadAssets reads every embedded static file.
func loadAssets() map[string]asset {
	entries, err := staticFS.ReadDir("static")
	if err != nil {
		panic("ui: reading embedded assets: " + err.Error()) // a build defect, caught by tests
	}
	out := make(map[string]asset, len(entries))
	for _, e := range entries {
		b, err := staticFS.ReadFile(path.Join("static", e.Name()))
		if err != nil {
			panic("ui: reading embedded asset: " + err.Error())
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = asset{body: b, ctype: contentType(e.Name()), hash: hex.EncodeToString(sum[:])}
	}
	return out
}

// assetURL returns the cache-busting URL of an embedded asset.
func (u *UI) assetURL(name string) string {
	return "/ui/static/" + name + "?v=" + u.assets[name].hash[:12]
}

// handleStatic serves an embedded asset. Assets are immutable for a given
// build, and their URLs carry the content hash.
func (u *UI) handleStatic(w http.ResponseWriter, r *http.Request) {
	a, ok := u.assets[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+a.hash+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.body))
}
