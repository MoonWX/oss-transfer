//go:build !embedded

package webui

import (
	"io/fs"
	"os"
)

func Assets() (fs.FS, error) {
	dir := os.Getenv("OSS_TRANSFER_WEB_DIR")
	if dir == "" {
		dir = "frontend/dist"
	}
	if _, err := os.Stat(dir + "/index.html"); err != nil {
		return nil, err
	}
	return os.DirFS(dir), nil
}
