package nodecontrol

import "context"

// ValidateConcurrency bounds the total active object requests on a node.
// Multipart requests also retain their existing per-upload bounds and node QPS.
func ValidateConcurrency(children, objects int) error {
	if children < 1 || children > 64 || objects < 1 || objects > 32 || children*objects > 256 {
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
	if err := c.commit(next, nil); err != nil {
		return Config{}, err
	}
	return c.config(), nil
}

func (c *Controller) config() Config {
	value := ConcurrencyConfig{MaxConcurrentChildren: 8, ObjectsPerChild: 4}
	if c.data.Concurrency != nil {
		value = *c.data.Concurrency
	}
	return Config{AdvertisedLocalRoots: append([]string{}, c.data.Roots...), ConcurrencyConfig: value}
}
