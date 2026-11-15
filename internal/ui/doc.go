// Package ui is the server-rendered web UI mounted at /ui/ (DESIGN.md §6.3).
//
// Pages are html/template files embedded in the binary, one per page plus a
// shared layout, parsed once in New. Small interactions (dashboard refresh,
// worker actions, confirmations) use htmx; every form also works without
// JavaScript.
//
// The UI owns no credentials or authorization logic of its own. Login,
// sessions and CSRF come from the server's seam (internal/server/session.go),
// every page and action authorizes with the same internal/auth functions the
// JSON API uses (mirroring internal/server/admin.go), and every change goes
// through the audited server.Ops. Controls the principal may not use are not
// rendered, and the handlers behind them refuse regardless.
//
// # Security
//
//   - Every response carries a strict Content-Security-Policy (no inline
//     script or style), frame-ancestors 'none', X-Frame-Options DENY,
//     nosniff and Referrer-Policy same-origin. htmx is configured through a
//     meta tag, with eval and script-tag processing disabled.
//   - Mutations are POST only, carry the session's CSRF token (form field or
//     header) and pass http.CrossOriginProtection (Sec-Fetch-Site / Origin),
//     which also covers the login form, where no session exists yet.
//   - Tokens and generated passwords are rendered once, in the response to
//     the POST that created them, with Cache-Control: no-store; they are
//     never logged.
//   - Login attempts are rate limited per client IP (the server's trusted
//     proxy rules apply) and failures are logged as auth.denied without the
//     submitted username or password.
//
// # Vendored htmx
//
// static/htmx.min.js is htmx 2.0.4, fetched from
// https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js (identical bytes on
// jsDelivr), sha256
// e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447. A test
// pins the hash; to upgrade, replace the file and update htmxVersion and
// htmxSHA256 in static.go.
package ui
