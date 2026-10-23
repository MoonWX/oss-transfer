package main

import (
	"io"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func compactAgentDatabase(path, destination string, progress io.Writer) error {
	return state.CompactOfflineWithOptions(path, "agent", destination, state.CompactOptions{
		Progress: progress,
		Normalize: func(bucketPath []string, key string, decoded []byte) ([]byte, error) {
			if len(bucketPath) == 1 && bucketPath[0] == "discovery-objects" {
				return discovery.CompactObjectIndexValue(key, decoded)
			}
			return decoded, nil
		},
	})
}
