// Package inventory stores immutable, deduplicated object-key manifests. Upload
// acknowledgement is durable; only a verified seal permits discovery.
package inventory

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/state"
)

const (
	MaxBatchKeys    = 128
	MaxKeyBytes     = 1024
	MaxRawKeys      = 10_000_000
	MaxContentBytes = 1 << 30
	MaxManifests    = 100_000
	MaxUploading    = 128
)

var (
	ErrInvalid  = errors.New("invalid inventory")
	ErrConflict = errors.New("inventory content or binding conflicts with the saved upload")
	ErrNotFound = errors.New("inventory not found")
	ErrUnsealed = errors.New("inventory upload is not sealed")
	ErrCapacity = errors.New("inventory upload capacity reached")
)

type Descriptor struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContentSHA256 string `json:"contentSha256"`
	Binding       string `json:"binding"`
	SpecSHA256    string `json:"specSha256"`
	ParentID      int64  `json:"parentId,omitempty"`
}
type Header struct {
	Descriptor
	State        string `json:"state"`
	NextBatch    int    `json:"nextBatch"`
	UniqueCount  int64  `json:"uniqueCount"`
	RawCount     int64  `json:"rawCount"`
	ContentBytes int64  `json:"contentBytes"`
	TaskID       int64  `json:"taskId,omitempty"`
}
type record struct {
	Header
	HashState []byte `json:"hashState"`
	Released  bool   `json:"released,omitempty"`
}
type Batch struct {
	ID     string   `json:"id"`
	Number int      `json:"number"`
	Keys   []string `json:"keys"`
}
type Seal struct {
	ID          string `json:"id"`
	BatchCount  int    `json:"batchCount"`
	UniqueCount int64  `json:"uniqueCount"`
}
type Repository struct{ store *state.Store }

func New(store *state.Store) *Repository { return &Repository{store} }
func ValidID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
func ValidateKey(key string) error {
	if key == "" || len(key) > MaxKeyBytes || !utf8.ValidString(key) || strings.ContainsAny(key, "\x00\r\n") {
		return fmt.Errorf("%w: keys must contain 1..1024 UTF-8 bytes without NUL or line breaks", ErrInvalid)
	}
	return nil
}
func validate(d Descriptor) error {
	if !ValidID(d.ID) || !ValidID(d.Binding) || !ValidID(d.SpecSHA256) || !ValidID(d.ContentSHA256) || d.Name == "" || len(d.Name) > 255 || !utf8.ValidString(d.Name) || strings.ContainsFunc(d.Name, unicode.IsControl) || strings.ContainsAny(d.Name, "/\\\x00\r\n") || d.ParentID < 0 {
		return fmt.Errorf("%w: descriptor", ErrInvalid)
	}
	return nil
}
func read(tx *state.Tx, id string) (record, error) {
	var h record
	found, err := tx.Get("inventory-manifests", id, &h)
	if err != nil {
		return h, err
	}
	if !found {
		return h, ErrNotFound
	}
	return h, nil
}
func (r *Repository) Get(ctx context.Context, id string) (h Header, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	err = r.store.View(func(tx *state.Tx) error { v, e := read(tx, id); h = v.Header; return e })
	return
}

// BeginTx permits the Master to bind owner/request metadata in the same commit.
func BeginTx(tx *state.Tx, d Descriptor) (Header, error) {
	if err := validate(d); err != nil {
		return Header{}, err
	}
	old, err := read(tx, d.ID)
	if err == nil {
		if old.Descriptor != d {
			return Header{}, ErrConflict
		}
		return old.Header, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Header{}, err
	}
	var counts struct{ Total, Uploading int }
	if _, err = tx.Get("inventory-meta", "counts", &counts); err != nil {
		return Header{}, err
	}
	if counts.Total >= MaxManifests || counts.Uploading >= MaxUploading {
		return Header{}, ErrCapacity
	}
	counts.Total++
	counts.Uploading++
	h := record{Header: Header{Descriptor: d, State: "UPLOADING", NextBatch: 1}}
	h.HashState, _ = sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err = tx.Put("inventory-meta", "counts", counts); err != nil {
		return Header{}, err
	}
	err = tx.Put("inventory-manifests", d.ID, h)
	return h.Header, err
}
func (r *Repository) Begin(ctx context.Context, d Descriptor) (h Header, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	err = r.store.Update(func(tx *state.Tx) error { h, err = BeginTx(tx, d); return err })
	return
}
func batchKey(id string, number int) string { return fmt.Sprintf("%s/%010d", id, number) }
func (r *Repository) Put(ctx context.Context, b Batch) (h Header, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if !ValidID(b.ID) || b.Number < 1 || b.Number > MaxRawKeys || len(b.Keys) < 1 || len(b.Keys) > MaxBatchKeys {
		return h, fmt.Errorf("%w: batches need 1..128 keys and a positive sequential number", ErrInvalid)
	}
	for _, key := range b.Keys {
		if err = ValidateKey(key); err != nil {
			return
		}
	}
	err = r.store.Update(func(tx *state.Tx) error {
		v, e := read(tx, b.ID)
		if e != nil {
			return e
		}
		h = v.Header
		if v.State == "CANCELLED" {
			return ErrConflict
		}
		var prior Batch
		found, e := tx.Get("inventory-batches", batchKey(b.ID, b.Number), &prior)
		if e != nil {
			return e
		}
		if found {
			a, _ := json.Marshal(prior)
			z, _ := json.Marshal(b)
			if string(a) != string(z) {
				return ErrConflict
			}
			return nil
		}
		if v.State != "UPLOADING" || b.Number != v.NextBatch {
			return ErrConflict
		}
		hasher := sha256.New()
		if e = hasher.(encoding.BinaryUnmarshaler).UnmarshalBinary(v.HashState); e != nil {
			return e
		}
		for _, key := range b.Keys {
			v.RawCount++
			v.ContentBytes += int64(len(key) + 1)
			if v.RawCount > MaxRawKeys || v.ContentBytes > MaxContentBytes {
				return fmt.Errorf("%w: maximum 10000000 raw keys or 1 GiB of key text per manifest", ErrCapacity)
			}
			_, _ = hasher.Write([]byte(key + "\n"))
			index := b.ID + "/" + base64.RawURLEncoding.EncodeToString([]byte(key))
			var old string
			found, e := tx.Get("inventory-keys", index, &old)
			if e != nil {
				return e
			}
			if !found {
				if e = tx.Put("inventory-keys", index, key); e != nil {
					return e
				}
				v.UniqueCount++
			}
		}
		v.NextBatch++
		v.HashState, e = hasher.(encoding.BinaryMarshaler).MarshalBinary()
		if e != nil {
			return e
		}
		if e = tx.Put("inventory-batches", batchKey(b.ID, b.Number), b); e != nil {
			return e
		}
		h = v.Header
		return tx.Put("inventory-manifests", b.ID, v)
	})
	return
}
func (r *Repository) Seal(ctx context.Context, request Seal) (h Header, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	err = r.store.Update(func(tx *state.Tx) error {
		v, e := read(tx, request.ID)
		if e != nil {
			return e
		}
		h = v.Header
		if v.State == "CANCELLED" {
			return ErrConflict
		}
		if request.BatchCount != v.NextBatch-1 || request.UniqueCount != v.UniqueCount {
			return fmt.Errorf("%w: batchCount or uniqueCount does not match the complete upload", ErrConflict)
		}
		if v.State != "UPLOADING" {
			return nil
		}
		hasher := sha256.New()
		if e = hasher.(encoding.BinaryUnmarshaler).UnmarshalBinary(v.HashState); e != nil {
			return e
		}
		if hex.EncodeToString(hasher.Sum(nil)) != v.ContentSHA256 {
			return fmt.Errorf("%w: contentSha256 does not match uploaded keys", ErrConflict)
		}
		var counts struct{ Total, Uploading int }
		if _, e = tx.Get("inventory-meta", "counts", &counts); e != nil {
			return e
		}
		if counts.Uploading <= 0 {
			return ErrConflict
		}
		counts.Uploading--
		v.State = "SEALED"
		v.HashState = nil
		h = v.Header
		if e = tx.Put("inventory-meta", "counts", counts); e != nil {
			return e
		}
		return tx.Put("inventory-manifests", request.ID, v)
	})
	return
}
func (r *Repository) Batch(ctx context.Context, id string, number int) (b Batch, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	err = r.store.View(func(tx *state.Tx) error {
		found, e := tx.Get("inventory-batches", batchKey(id, number), &b)
		if e == nil && !found {
			return ErrNotFound
		}
		return e
	})
	return
}
func (r *Repository) Keys(ctx context.Context, id, after string, limit int) (keys []string, next string, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if limit < 1 || limit > MaxBatchKeys {
		return nil, "", ErrInvalid
	}
	err = r.store.View(func(tx *state.Tx) error {
		h, e := read(tx, id)
		if e != nil {
			return e
		}
		if !Ready(h.Header) {
			return ErrUnsealed
		}
		return tx.Scan("inventory-keys", id+"/", after, limit, func(index string, raw json.RawMessage) error {
			var key string
			if e := json.Unmarshal(raw, &key); e != nil {
				return e
			}
			keys = append(keys, key)
			next = index
			return nil
		})
	})
	return
}

// BindTaskTx is called in the transaction that creates the parent and its
// idempotency record; a lost finalize response cannot create a second task.
func BindTaskTx(tx *state.Tx, id string, taskID int64, specSHA256 string) error {
	v, err := read(tx, id)
	if err != nil {
		return err
	}
	if !Ready(v.Header) {
		return ErrUnsealed
	}
	if v.ParentID != 0 || v.SpecSHA256 != specSHA256 || v.TaskID != 0 && v.TaskID != taskID {
		return ErrConflict
	}
	v.State = "FINALIZED"
	v.TaskID = taskID
	return tx.Put("inventory-manifests", id, v)
}

// Ready rejects cancelled as well as unfinished uploads.
func Ready(h Header) bool { return h.State == "SEALED" || h.State == "FINALIZED" }
func (r *Repository) Require(ctx context.Context, id string, parentID int64) error {
	h, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	if h.ParentID != parentID {
		return ErrConflict
	}
	if !Ready(h) {
		return ErrUnsealed
	}
	return nil
}

// Cancel fences an unbound upload before deleting its data in bounded commits.
// Retrying after interruption completes cleanup; the ID remains a tombstone.
func (r *Repository) Cancel(ctx context.Context, id string) (h Header, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	err = r.store.Update(func(tx *state.Tx) error {
		v, e := read(tx, id)
		if e != nil {
			return e
		}
		if v.TaskID > 0 || v.ParentID > 0 {
			return ErrConflict
		}
		if v.State == "CANCELLED" {
			h = v.Header
			return nil
		}
		var counts struct{ Total, Uploading int }
		if _, e = tx.Get("inventory-meta", "counts", &counts); e != nil {
			return e
		}
		if v.State == "UPLOADING" {
			if counts.Uploading <= 0 {
				return ErrConflict
			}
			counts.Uploading--
		}
		v.State = "CANCELLED"
		v.HashState = nil
		h = v.Header
		if e = tx.Put("inventory-meta", "counts", counts); e != nil {
			return e
		}
		return tx.Put("inventory-manifests", id, v)
	})
	if err != nil {
		return
	}
	for _, bucket := range []string{"inventory-batches", "inventory-keys"} {
		for {
			if err = ctx.Err(); err != nil {
				return
			}
			count := 0
			err = r.store.Update(func(tx *state.Tx) error {
				keys := make([]string, 0, 256)
				if e := tx.Scan(bucket, id+"/", "", 256, func(k string, _ json.RawMessage) error { keys = append(keys, k); return nil }); e != nil {
					return e
				}
				count = len(keys)
				for _, key := range keys {
					if e := tx.Delete(bucket, key); e != nil {
						return e
					}
				}
				return nil
			})
			if err != nil {
				return
			}
			if count < 256 {
				break
			}
		}
	}
	err = r.store.Update(func(tx *state.Tx) error {
		v, e := read(tx, id)
		if e != nil {
			return e
		}
		h = v.Header
		if v.Released {
			return nil
		}
		var counts struct{ Total, Uploading int }
		if _, e = tx.Get("inventory-meta", "counts", &counts); e != nil {
			return e
		}
		if counts.Total <= 0 {
			return ErrConflict
		}
		counts.Total--
		v.Released = true
		if e = tx.Put("inventory-meta", "counts", counts); e != nil {
			return e
		}
		return tx.Put("inventory-manifests", id, v)
	})
	return
}
