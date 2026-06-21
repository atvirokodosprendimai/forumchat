// Package web embeds the application's static assets so the binary is
// self-contained — it no longer depends on ./web/static existing on disk at
// runtime. Asset access (versioning, inlining, file serving) all reads from
// Static.
package web

import "embed"

// Static holds everything under web/static (app.css, the JS helpers, icons,
// the web manifest and the service worker). Files are addressed with a
// "static/" prefix, e.g. Static.ReadFile("static/app.css").
//
//go:embed static
var Static embed.FS
