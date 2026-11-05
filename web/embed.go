// Package web holds Cerberus' embedded single-page web UI.
//
// The files live under dist/: dist/index.html is the app shell and every
// static asset (scripts, styles, fonts, vendored xterm.js) sits under
// dist/assets/. The shell links its assets relatively ("assets/..."), so it
// works when served at "/" or "/login". The API is expected to do:
//
//	ui, _ := fs.Sub(web.FS, "dist")
//
// then serve ui's index.html at "/" (and "/login", unauthenticated) with
// Cache-Control: no-cache, and ui's assets/ tree at "/assets/" without auth,
// which matches the auth exemptions in docs/SCOPE.md.
package web

import "embed"

// FS contains the UI files rooted at "dist/".
//
//go:embed all:dist
var FS embed.FS
