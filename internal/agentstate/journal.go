// Package agentstate stores Agent command outcomes durably. It prevents blind
// mutation replay after restart; it cannot make external storage writes atomic
// with the local journal or guarantee exactly-once external side effects.
package agentstate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
)

const recordsBucket = "agent_command_journal"
const metadataBucket = "agent_command_journal_meta"

var ErrUnknownOutcome = errors.New("command outcome is unknown; reconciliation is required")
var ErrCorruptJournal = errors.New("agent command journal metadata is invalid")

type Options struct {
	MaxEntries       int64
	MaxResponseBytes int64
}
type Stats struct {
	Entries       int64 `json:"entries"`
	ResponseBytes int64 `json:"responseBytes"`
}
type record struct {
	Fingerprint string                      `json:"fingerprint"`
	BootID      string                      `json:"bootId"`
	State       agenttransport.JournalState `json:"state"`
	Response    agenttransport.Response     `json:"response"`
}

// Journal should be constructed once per Agent process, on its exclusive Store.
// Each instance is a new boot: old unfinished claims become unknown on access.
// Records are not evicted to make room; pruning requires application-specific
// proof that the corresponding idempotency keys can never be delivered again.
type Journal struct {
	store   *state.Store
	bootID  string
	options Options
}

var _ agenttransport.Journal = (*Journal)(nil)

func NewJournal(store *state.Store, options Options) (*Journal, error) {
	if store == nil {
		return nil, errors.New("agent journal requires a state store")
	}
	if options.MaxEntries <= 0 {
		options.MaxEntries = 100000
	}
	if options.MaxResponseBytes <= 0 {
		options.MaxResponseBytes = 64 << 20
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	j := &Journal{store: store, bootID: hex.EncodeToString(id[:]), options: options}
	err := store.Update(func(tx *state.Tx) error {
		var usage Stats
		found, err := tx.Get(metadataBucket, "usage", &usage)
		if err != nil {
			return err
		}
		if found {
			if usage.Entries < 0 || usage.ResponseBytes < 0 {
				return ErrCorruptJournal
			}
			return nil
		}
		hasRecords := false
		if err = tx.Scan(recordsBucket, "", "", 1, func(string, json.RawMessage) error { hasRecords = true; return nil }); err != nil {
			return err
		}
		if hasRecords {
			return ErrCorruptJournal
		}
		return tx.Put(metadataBucket, "usage", usage)
	})
	if err != nil {
		return nil, err
	}
	return j, nil
}
func validClaim(key, fingerprint string) bool {
	return key != "" && len(key) <= 2048 && fingerprint != "" && len(fingerprint) <= 256
}
func usage(tx *state.Tx) (Stats, error) {
	var value Stats
	found, err := tx.Get(metadataBucket, "usage", &value)
	if err != nil {
		return value, err
	}
	if !found || value.Entries < 0 || value.ResponseBytes < 0 {
		return value, ErrCorruptJournal
	}
	return value, nil
}
func (j *Journal) Begin(ctx context.Context, key, fingerprint string) (agenttransport.Claim, error) {
	if !validClaim(key, fingerprint) {
		return agenttransport.Claim{}, agenttransport.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return agenttransport.Claim{}, err
	}
	var claim agenttransport.Claim
	err := j.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var entry record
		found, err := tx.Get(recordsBucket, key, &entry)
		if err != nil {
			return err
		}
		if found {
			if entry.Fingerprint != fingerprint {
				return agenttransport.ErrIdempotencyConflict
			}
			switch entry.State {
			case agenttransport.JournalCompleted:
			case agenttransport.JournalUnknown:
			case agenttransport.JournalRunning:
				if entry.BootID != j.bootID {
					entry.State = agenttransport.JournalUnknown
					if err = tx.Put(recordsBucket, key, entry); err != nil {
						return err
					}
				}
			default:
				return ErrCorruptJournal
			}
			claim = agenttransport.Claim{State: entry.State, Response: entry.Response}
			return ctx.Err()
		}
		count, err := usage(tx)
		if err != nil {
			return err
		}
		if count.Entries >= j.options.MaxEntries {
			return agenttransport.ErrJournalFull
		}
		count.Entries++
		if err = tx.Put(metadataBucket, "usage", count); err != nil {
			return err
		}
		entry = record{Fingerprint: fingerprint, BootID: j.bootID, State: agenttransport.JournalRunning}
		if err = tx.Put(recordsBucket, key, entry); err != nil {
			return err
		}
		claim = agenttransport.Claim{Acquired: true, State: agenttransport.JournalRunning}
		return ctx.Err()
	})
	if err != nil {
		return agenttransport.Claim{}, err
	}
	return claim, nil
}
func (j *Journal) Complete(ctx context.Context, key, fingerprint string, response agenttransport.Response) error {
	if !validClaim(key, fingerprint) || response.Status < 100 || response.Status > 599 {
		return agenttransport.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var entry record
		found, err := tx.Get(recordsBucket, key, &entry)
		if err != nil {
			return err
		}
		if !found || entry.Fingerprint != fingerprint {
			return agenttransport.ErrIdempotencyConflict
		}
		if entry.State == agenttransport.JournalCompleted {
			storedJSON, storedErr := json.Marshal(entry.Response)
			responseJSON, responseErr := json.Marshal(response)
			if storedErr == nil && responseErr == nil && bytes.Equal(storedJSON, responseJSON) {
				return nil
			}
			return agenttransport.ErrIdempotencyConflict
		}
		if entry.State != agenttransport.JournalRunning || entry.BootID != j.bootID {
			return ErrUnknownOutcome
		}
		count, err := usage(tx)
		if err != nil {
			return err
		}
		delta := int64(len(response.Payload)) - int64(len(entry.Response.Payload))
		if delta > 0 && delta > j.options.MaxResponseBytes-count.ResponseBytes {
			return agenttransport.ErrJournalFull
		}
		count.ResponseBytes += delta
		if count.ResponseBytes < 0 {
			return ErrCorruptJournal
		}
		// A serialization or commit failure rolls metadata and the record back together.
		if err = tx.Put(metadataBucket, "usage", count); err != nil {
			return err
		}
		entry.State = agenttransport.JournalCompleted
		entry.Response = response
		if err = tx.Put(recordsBucket, key, entry); err != nil {
			return err
		}
		return ctx.Err()
	})
}
func (j *Journal) Unknown(ctx context.Context, key, fingerprint string) error {
	if !validClaim(key, fingerprint) {
		return agenttransport.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.store.Update(func(tx *state.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var entry record
		found, err := tx.Get(recordsBucket, key, &entry)
		if err != nil {
			return err
		}
		if !found || entry.Fingerprint != fingerprint {
			return agenttransport.ErrIdempotencyConflict
		}
		if entry.State == agenttransport.JournalCompleted || entry.State == agenttransport.JournalUnknown {
			return nil
		}
		if entry.State != agenttransport.JournalRunning {
			return ErrCorruptJournal
		}
		entry.State = agenttransport.JournalUnknown
		if err = tx.Put(recordsBucket, key, entry); err != nil {
			return err
		}
		return ctx.Err()
	})
}
func (j *Journal) Stats() (Stats, error) {
	var value Stats
	err := j.store.View(func(tx *state.Tx) error { var err error; value, err = usage(tx); return err })
	return value, err
}
