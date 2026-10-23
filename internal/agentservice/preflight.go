package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Validation does not grant an execution job a shared-storage proof or modify
// its generation fence.
type PreflightProbeRequest struct {
	Root   string                    `json:"root"`
	Action string                    `json:"action"`
	Probe  transferstore.SharedProbe `json:"probe"`
}

const preflightProbeBucket = "agent-preflight-probes"

type preflightProbeRecord struct {
	Root         string                    `json:"root"`
	RootIdentity string                    `json:"rootIdentity"`
	Probe        transferstore.SharedProbe `json:"probe"`
	ExpiresAt    time.Time                 `json:"expiresAt"`
	Closed       bool                      `json:"closed"`
	Cleaned      bool                      `json:"cleaned"`
}

type preflightProbeLock struct {
	token chan struct{}
	users int
}

// Lifecycles stay in the Agent database. Closed records fence late writes;
// pending records identify the exact directory and nonce to recover. A stalled
// mount can hold only its own probe lock, not all validation commands.
type preflightProbes struct {
	store *state.Store
	mu    sync.Mutex
	locks map[string]*preflightProbeLock
}

func (p *preflightProbes) lock(ctx context.Context, key string) (func(), error) {
	p.mu.Lock()
	if p.locks == nil {
		p.locks = map[string]*preflightProbeLock{}
	}
	lock := p.locks[key]
	if lock == nil {
		lock = &preflightProbeLock{token: make(chan struct{}, 1)}
		p.locks[key] = lock
	}
	lock.users++
	p.mu.Unlock()
	releaseRef := func() {
		p.mu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(p.locks, key)
		}
		p.mu.Unlock()
	}
	select {
	case lock.token <- struct{}{}:
		return func() { <-lock.token; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

func (p *preflightProbes) execute(ctx context.Context, request PreflightProbeRequest) error {
	if !filepath.IsAbs(request.Root) {
		return errors.New("shared storage preflight requires an absolute directory")
	}
	if err := transferstore.ValidateSharedProbe(request.Probe); err != nil {
		return err
	}
	if request.Action == "read" {
		return transferstore.VerifySharedProbe(ctx, request.Root, request.Probe)
	}
	if request.Action != "write" && request.Action != "remove" {
		return errors.New("invalid shared storage preflight action")
	}
	unlock, err := p.lock(ctx, request.Probe.Key)
	if err != nil {
		return err
	}
	defer unlock()
	var record preflightProbeRecord
	var found bool
	if err := p.store.View(func(tx *state.Tx) (err error) {
		found, err = tx.Get(preflightProbeBucket, request.Probe.Key, &record)
		return err
	}); err != nil {
		return err
	}
	if found && (record.Root != request.Root || record.Probe != request.Probe) {
		return transferstore.ErrSharedProbe
	}
	if !found {
		record = preflightProbeRecord{Root: request.Root, Probe: request.Probe, ExpiresAt: time.Now().UTC().Add(2 * time.Minute)}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	save := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return p.store.Update(func(tx *state.Tx) error { return tx.Put(preflightProbeBucket, request.Probe.Key, record) })
	}
	if request.Action == "write" {
		if record.Closed || time.Now().After(record.ExpiresAt) {
			return errors.New("shared storage preflight has already ended")
		}
		root, err := transferstore.OpenSharedProbeRoot(record.Root)
		if err != nil {
			return err
		}
		defer root.Close()
		if found && record.RootIdentity != root.Identity() {
			return transferstore.ErrSharedProbe
		}
		record.RootIdentity = root.Identity()
		// Persist the pinned directory identity before any marker write.
		if err := save(); err != nil {
			return err
		}
		return root.Ensure(ctx, request.Probe)
	}

	record.Closed = true
	if !found {
		// No write was ever registered. Fence a write arriving after cleanup.
		record.Cleaned = true
	}
	if err := save(); err != nil {
		return err
	}
	if record.Cleaned {
		return nil
	}
	root, err := transferstore.OpenSharedProbeRoot(record.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	if root.Identity() != record.RootIdentity {
		return transferstore.ErrSharedProbe
	}
	if err := root.Cleanup(ctx, request.Probe); err != nil {
		return err
	}
	record.Cleaned = true
	return save()
}

func (p *preflightProbes) records(after string) ([]preflightProbeRecord, string, error) {
	var records []preflightProbeRecord
	err := p.store.View(func(tx *state.Tx) error {
		return tx.Scan(preflightProbeBucket, "", after, 128, func(key string, raw json.RawMessage) error {
			var record preflightProbeRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if key != record.Probe.Key {
				return transferstore.ErrSharedProbe
			}
			records = append(records, record)
			after = key
			return nil
		})
	})
	return records, after, err
}

// Startup closes every old lifecycle using only the local database. It must
// never touch remote mounts before the Agent connects to its Master.
func (p *preflightProbes) fencePending() error {
	after := ""
	for {
		records, next, err := p.records(after)
		if err != nil {
			return err
		}
		if err := p.store.Update(func(tx *state.Tx) error {
			for _, record := range records {
				if !record.Closed {
					record.Closed = true
					if err := tx.Put(preflightProbeBucket, record.Probe.Key, record); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if len(records) < 128 {
			return nil
		}
		after = next
	}
}

func (p *preflightProbes) sweep(ctx context.Context) error {
	after := ""
	for {
		records, next, err := p.records(after)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !record.Closed && time.Now().Before(record.ExpiresAt) {
				continue
			}
			if err := p.execute(ctx, PreflightProbeRequest{Root: record.Root, Probe: record.Probe, Action: "remove"}); err != nil {
				continue // Keep the exact directory identity until its mount returns.
			}
			// Commands expire after 30 seconds. Keep a longer fence so delayed
			// retries cannot recreate a removed probe.
			if time.Now().After(record.ExpiresAt.Add(5 * time.Minute)) {
				if err := p.store.Update(func(tx *state.Tx) error { return tx.Delete(preflightProbeBucket, record.Probe.Key) }); err != nil {
					return err
				}
			}
		}
		if len(records) < 128 {
			return nil
		}
		after = next
	}
}

func (p *preflightProbes) run(ctx context.Context) {
	// This worker is deliberately excluded from Service.Close's wait group:
	// cancellation cannot interrupt a filesystem syscall on an offline NFS.
	_ = p.sweep(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.sweep(ctx)
		}
	}
}
