// Package web embeds the built local dashboard (web/dist) into the daemon
// binary. dist/ is committed so `go build` works from a fresh clone; run
// `npm install && npm run build` in web/ to regenerate it from the React
// source (falls back to the hand-written static page otherwise).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the dashboard filesystem rooted at the bundle.
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
