//go:build embedded

package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:assets
var files embed.FS

func Assets() (fs.FS, error) { return fs.Sub(files, "assets") }
