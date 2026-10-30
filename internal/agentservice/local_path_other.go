//go:build !linux

package agentservice

import (
	"context"
	"os"
)

func statPreflightPath(ctx context.Context, path string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Stat(path)
}
