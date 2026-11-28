// Package web embeds the compiled Vite/React production bundle (web/dist) directly
// into the Go binary so cmd/reconcilex serves both the REST API and UI from a
// single container without Nginx or a runtime Node.js process.
package web

import (
	"embed"
	"io/fs"
)

// embeddedDist captures every file inside web/dist at compile time.
// The "all:" prefix includes dotfiles such as web/dist/.gitkeep so Go builds
// succeed even on a fresh checkout before `npm run build` populates assets.
//
//go:embed all:dist
var embeddedDist embed.FS

// DistFS returns a read-only sub-filesystem rooted at "dist" (stripping the "dist/"
// path prefix) so internal/api can resolve "index.html" and "assets/*" directly.
func DistFS() (fs.FS, error) {
	return fs.Sub(embeddedDist, "dist")
}
