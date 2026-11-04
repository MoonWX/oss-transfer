//go:build !linux && !darwin

package transferstore

import "os"

// orderedFileSync runs before a write whose durability is deferred to
// Local.Sync is published. Without a cheaper ordering primitive, the payload
// is made fully durable before it becomes visible.
func orderedFileSync(file *os.File, _ *orderingCache) error { return file.Sync() }
