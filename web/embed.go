package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embeddedDist embed.FS

// DistFS returns the compiled Vite frontend bundle rooted at web/dist.
func DistFS() (fs.FS, error) {
	return fs.Sub(embeddedDist, "dist")
}
