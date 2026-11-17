package nodecontrol

import (
	"context"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// ValidateConcurrency bounds the total active object requests on a node.
// Multipart requests also retain their existing per-upload bounds and node QPS.
func ValidateConcurrency(children, objects int) error {
	if children < 1 || children > 256 || objects < 1 || objects > 64 || children*objects > 1024 {
		return ErrInvalidConcurrency
	}
	return nil
}

// Scan concurrency is how many scans a node runs at once. Every Agent before
// it was configurable ran DefaultScanConcurrency.
const (
	DefaultScanConcurrency = 2
	MaxScanConcurrency     = 16
)

// ValidateScanConcurrency bounds the scans a node runs at once. Each scan
// keeps source requests in flight for as long as it lists, so the bound is
// kept low.
func ValidateScanConcurrency(scans int) error {
	if scans < 1 || scans > MaxScanConcurrency {
		return ErrInvalid
	}
	return nil
}

func (c *Controller) SavedConcurrency() (ConcurrencyConfig, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.Concurrency == nil {
		return ConcurrencyConfig{}, false
	}
	return *c.data.Concurrency, true
}

// SavedScanConcurrency returns the scan concurrency saved on this node, if
// any. New has already dropped a saved value that is not valid.
func (c *Controller) SavedScanConcurrency() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.ScanConcurrency == nil {
		return 0, false
	}
	return *c.data.ScanConcurrency, true
}

func (c *Controller) UpdateConfig(ctx context.Context, change ConfigChange) (Config, error) {
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.healthy(); err != nil {
		return Config{}, err
	}
	next := clone(c.data)
	if change.AdvertisedLocalRoots != nil {
		roots, err := validatedRoots(*change.AdvertisedLocalRoots)
		if err != nil {
			return Config{}, err
		}
		next.Roots = roots
	}
	if change.MaxConcurrentChildren != nil || change.ObjectsPerChild != nil {
		value := c.config().ConcurrencyConfig
		if change.MaxConcurrentChildren != nil {
			value.MaxConcurrentChildren = *change.MaxConcurrentChildren
		}
		if change.ObjectsPerChild != nil {
			value.ObjectsPerChild = *change.ObjectsPerChild
		}
		if err := ValidateConcurrency(value.MaxConcurrentChildren, value.ObjectsPerChild); err != nil {
			return Config{}, err
		}
		next.Concurrency = &value
	}
	if change.MaxConcurrentScans != nil {
		scans := *change.MaxConcurrentScans
		if err := ValidateScanConcurrency(scans); err != nil {
			return Config{}, err
		}
		next.ScanConcurrency = &scans
	}
	if err := c.commit(next, nil); err != nil {
		return Config{}, err
	}
	return c.config(), nil
}

func (c *Controller) config() Config {
	value := ConcurrencyConfig{MaxConcurrentChildren: execution.DefaultChildConcurrency, ObjectsPerChild: execution.DefaultObjectConcurrency}
	if c.data.Concurrency != nil {
		value = *c.data.Concurrency
	}
	scans := DefaultScanConcurrency
	if c.data.ScanConcurrency != nil {
		scans = *c.data.ScanConcurrency
	}
	return Config{AdvertisedLocalRoots: append([]string{}, c.data.Roots...), ConcurrencyConfig: value, MaxConcurrentScans: scans}
}
