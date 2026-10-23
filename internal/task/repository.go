package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type Repository struct{ store *state.Store }

func New(store *state.Store) *Repository   { return &Repository{store} }
func key(id int64) string                  { return fmt.Sprintf("%020d", id) }
func childKey(id int64, number int) string { return fmt.Sprintf("%020d/%010d", id, number) }
func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func get(tx *state.Tx, id int64) (Parent, error) {
	var p Parent
	ok, err := tx.Get("master-tasks", key(id), &p)
	if err == nil && !ok {
		err = ErrNotFound
	}
	return p, err
}
func (r *Repository) Get(id int64) (p Parent, err error) {
	err = r.store.View(func(tx *state.Tx) error { p, err = get(tx, id); return err })
	return
}
func (r *Repository) RuntimeProblem(id int64, generation uint64, message string) error {
	if len(message) > 4096 {
		message = message[:4096]
	}
	return unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.RuntimeError == message {
			return errUnchanged
		}
		p.RuntimeError = message
		return save(tx, p)
	}))
}
func validate(spec Spec) error {
	if spec.InventoryID != "" && (!inventory.ValidID(spec.InventoryID) || spec.Direction != "OSS_TO_OSS" || spec.Prefix != "" || spec.ObjectKey != "" || spec.StartKey != "" || spec.EndKey != "" || spec.ModifiedFrom != nil || spec.ModifiedUntil != nil) {
		return inventory.ErrInvalid
	}
	if err := execution.ValidateRequestLimits(spec.RequestLimits); err != nil {
		return err
	}
	if len(spec.Nodes) < 1 || len(spec.Nodes) > 1024 {
		return errors.New("select between 1 and 1024 Agents")
	}
	if limits := spec.RequestLimits; limits != nil && limits.Scope != "PER_NODE" && limits.BandwidthBitsPerSecond > 0 && limits.BandwidthBitsPerSecond/8/float64(len(spec.Nodes)) < 1 {
		return errors.New("bandwidth limit must provide at least one byte per second per selected Agent")
	}
	seen := map[string]bool{}
	for _, node := range spec.Nodes {
		if node.ID == "" || node.EnrollmentID == "" || seen[node.ID] {
			return errors.New("invalid or duplicate Agent")
		}
		seen[node.ID] = true
	}
	if spec.ConflictPolicy != "SKIP" && spec.ConflictPolicy != "OVERWRITE" {
		return errors.New("conflict policy must be SKIP or OVERWRITE")
	}
	if spec.DistributionMode != "BALANCED" && spec.DistributionMode != "AFFINITY" {
		return errors.New("distribution mode must be BALANCED or AFFINITY")
	}
	if math.IsNaN(spec.BandwidthMbps) || math.IsInf(spec.BandwidthMbps, 0) || spec.BandwidthMbps < 0 || spec.BandwidthMbps > 1e9 {
		return errors.New("invalid bandwidth limit")
	}
	switch spec.Direction {
	case "OSS_TO_OSS":
		if spec.Source.Kind != "S3" || spec.Destination.Kind != "S3" {
			return errors.New("invalid OSS endpoints")
		}
	case "OSS_TO_LOCAL":
		if spec.Source.Kind != "S3" || spec.Destination.Kind != "LOCAL" {
			return errors.New("invalid download endpoints")
		}
	case "LOCAL_TO_OSS":
		if spec.Source.Kind != "LOCAL" || spec.Destination.Kind != "S3" {
			return errors.New("invalid upload endpoints")
		}
	default:
		return errors.New("unsupported transfer direction")
	}
	if spec.Source == spec.Destination {
		return errors.New("source and destination must differ")
	}
	if spec.Prefix != "" && spec.ObjectKey != "" {
		return errors.New("prefix and object key cannot both be set")
	}
	if spec.TargetKey != "" && (spec.ObjectKey == "" || spec.TargetPrefix != "") {
		return errors.New("a destination key requires one source object")
	}
	switch spec.MappingMode {
	case "", "PRESERVE", "ADD_PREFIX", "EXACT_KEY":
	case "REPLACE_PREFIX":
		if spec.Prefix == "" || spec.ReplaceSourcePrefix != spec.Prefix || spec.TargetKey != "" {
			return errors.New("replacement source prefix must equal the selected source prefix")
		}
	default:
		return errors.New("unknown key mapping")
	}
	if spec.ObjectKey != "" && (spec.StartKey != "" || spec.EndKey != "") {
		return errors.New("a single object cannot have a key range")
	}
	if spec.EndKey != "" && spec.StartKey >= spec.EndKey {
		return errors.New("end key must follow start key")
	}
	if spec.ModifiedFrom != nil && spec.ModifiedUntil != nil && !spec.ModifiedFrom.Before(*spec.ModifiedUntil) {
		return errors.New("invalid modification time range")
	}
	return nil
}

// Create binds a caller's request key to exactly one immutable task definition.
// The key is separate from the definition so intentionally repeated migrations
// can still create different tasks.
func (r *Repository) Create(spec Spec, requestKey string) (Parent, bool, error) {
	return r.CreateChecked(spec, requestKey, nil)
}

// CreateChecked evaluates authorization/configuration preconditions in the same
// transaction as the immutable parent and its idempotency record.
func (r *Repository) CreateChecked(spec Spec, requestKey string, check func(*state.Tx) error) (p Parent, created bool, err error) {
	if err = validate(spec); err != nil {
		return
	}
	if requestKey == "" || len(requestKey) > 256 {
		err = errors.New("an idempotency key is required")
		return
	}
	fingerprint := digest(spec)
	err = r.store.Update(func(tx *state.Tx) error {
		if check != nil {
			if err := check(tx); err != nil {
				return err
			}
		}
		var id int64
		found, err := tx.Get("master-task-requests", digest(requestKey), &id)
		if err != nil {
			return err
		}
		if found {
			p, err = get(tx, id)
			if err != nil {
				return err
			}
			if p.Fingerprint != fingerprint {
				return ErrConflict
			}
			return nil
		}
		next, err := tx.NextID("master-tasks")
		if err != nil {
			return err
		}
		if next > 1<<53-1 {
			return errors.New("task ID space exhausted")
		}
		now := time.Now().UTC()
		p = Parent{ID: int64(next), Spec: spec, Generation: 1, BudgetRevision: 1, Status: "RUNNING", CreatedAt: now, StartedAt: &now, Fingerprint: fingerprint}
		if spec.InventoryID != "" {
			if err := inventory.BindTaskTx(tx, spec.InventoryID, p.ID, fingerprint); err != nil {
				return err
			}
		}
		if err := save(tx, p); err != nil {
			return err
		}
		if err := tx.Put("master-task-requests", digest(requestKey), p.ID); err != nil {
			return err
		}
		created = true
		return nil
	})
	return
}

func save(tx *state.Tx, p Parent) error {
	if err := tx.Put("master-tasks", key(p.ID), p); err != nil {
		return err
	}
	if err := syncListingIndex(tx, p); err != nil {
		return err
	}
	if !p.Deleted && !Ended(p.Status) {
		return tx.Put("master-task-active", key(p.ID), p.ID)
	}
	return tx.Delete("master-task-active", key(p.ID))
}
func (r *Repository) List(after int64, limit int, activeOnly bool) (result []Parent, err error) {
	if limit < 1 || limit > 256 {
		return nil, errors.New("page size must be between 1 and 256")
	}
	bucket := "master-tasks"
	if activeOnly {
		bucket = "master-task-active"
	}
	err = r.store.View(func(tx *state.Tx) error {
		return tx.Scan(bucket, "", key(after), limit, func(k string, raw json.RawMessage) error {
			var p Parent
			if activeOnly {
				var id int64
				if err := json.Unmarshal(raw, &id); err != nil {
					return err
				}
				var err error
				p, err = get(tx, id)
				if err != nil {
					return err
				}
			} else if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			result = append(result, p)
			return nil
		})
	})
	return
}

// Assignment is one durable scanner batch awaiting node assignment.
type Assignment struct {
	Number  int
	Objects []transferstore.Object
	Legacy  map[string]execution.LegacyCandidate
}

// Assign commits the scanner batch and its node assignment before any command
// can be sent to an Agent. A retried batch returns that original assignment.
func (r *Repository) Assign(id int64, generation uint64, number int, objects []transferstore.Object, legacy ...map[string]execution.LegacyCandidate) (Child, error) {
	if len(legacy) > 1 {
		return Child{}, errors.New("invalid discovery batch")
	}
	batch := Assignment{Number: number, Objects: objects}
	if len(legacy) == 1 {
		batch.Legacy = legacy[0]
	}
	children, err := r.AssignPage(id, generation, []Assignment{batch})
	if err != nil {
		return Child{}, err
	}
	return children[0], nil
}

// AssignPage records a bounded dispatch page in one transaction, before any
// Agent is called or scanner checkpoint acknowledged. Partial delivery remains
// recoverable from the outstanding-child index after a crash or disconnection.
func (r *Repository) AssignPage(id int64, generation uint64, batches []Assignment) ([]Child, error) {
	if len(batches) == 0 || len(batches) > 16 {
		return nil, errors.New("assignment page must contain 1 to 16 batches")
	}
	for _, batch := range batches {
		if batch.Number < 1 || len(batch.Objects) < 1 || len(batch.Objects) > 128 {
			return nil, errors.New("invalid discovery batch")
		}
	}
	children := make([]Child, 0, len(batches))
	err := r.store.Update(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.Status != "RUNNING" || p.Deleted {
			return ErrState
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		var other map[string]PendingLoad
		if p.Spec.DistributionMode == "BALANCED" && len(p.Spec.Nodes) > 1 {
			other, err = otherPending(tx, p)
			if err != nil {
				return err
			}
		}
		for _, batch := range batches {
			child, err := assignChild(tx, &p, batch.Number, batch.Objects, batch.Legacy, other)
			if err != nil {
				return err
			}
			children = append(children, child)
		}
		return save(tx, p)
	})
	if err != nil {
		return nil, err
	}
	return children, nil
}

func assignChild(tx *state.Tx, p *Parent, number int, objects []transferstore.Object, legacy map[string]execution.LegacyCandidate, other map[string]PendingLoad) (child Child, err error) {
	found, err := tx.Get("master-task-children", childKey(p.ID, number), &child)
	if err != nil {
		return child, err
	}
	if found {
		if len(child.Job.Items) != len(objects) {
			return child, ErrConflict
		}
		for index, item := range child.Job.Items {
			var candidate *execution.LegacyCandidate
			if legacy != nil {
				if value, ok := legacy[objects[index].Key]; ok {
					candidate = &value
				}
			}
			if digest(item.Source) != digest(objects[index]) || !reflect.DeepEqual(item.Legacy, candidate) {
				return child, ErrConflict
			}
		}
		return child, nil
	}
	if number != p.DispatchedThrough+1 {
		return child, errors.New("discovery batch arrived out of order")
	}
	node, err := chooseNode(tx, *p, other)
	if err != nil {
		return child, err
	}
	job := execution.Job{RequestLimits: p.Spec.RequestLimits, ChildID: fmt.Sprintf("%d-%d", p.ID, number), ParentID: p.ID, Number: number, Generation: p.Generation, Source: p.Spec.Source, Destination: p.Spec.Destination, ConflictPolicy: p.Spec.ConflictPolicy, DiscoveryComplete: true}
	job.SharedLocalSource = p.Spec.SharedLocalSource
	if job.Source.Kind == "LOCAL" {
		job.Source.Root = node.LocalRoot
	}
	if job.Destination.Kind == "LOCAL" {
		job.Destination.Root = node.LocalRoot
	}
	var bytes int64
	for _, object := range objects {
		if object.Size < 0 || bytes > math.MaxInt64-object.Size {
			return child, errors.New("invalid object size")
		}
		target := object.Key
		if p.Spec.TargetKey != "" {
			target = p.Spec.TargetKey
		} else if p.Spec.MappingMode == "REPLACE_PREFIX" {
			if !strings.HasPrefix(object.Key, p.Spec.ReplaceSourcePrefix) {
				return child, errors.New("source key does not match replacement prefix")
			}
			target = p.Spec.TargetPrefix + strings.TrimPrefix(object.Key, p.Spec.ReplaceSourcePrefix)
		} else if p.Spec.TargetPrefix != "" {
			target = p.Spec.TargetPrefix
			if !strings.HasSuffix(target, "/") {
				target += "/"
			}
			target += object.Key
		}
		item := execution.Item{ID: digest(object.Key), Source: object, DestinationKey: target}
		if legacy != nil {
			if candidate, ok := legacy[object.Key]; ok {
				if p.Legacy == nil || !p.Legacy.RecoveryStarted {
					return child, ErrConflict
				}
				item.Legacy = &candidate
			}
		}
		job.Items = append(job.Items, item)
		bytes += object.Size
	}
	child = Child{Job: job, Node: node}
	pending, err := addLoad(p.Scheduling.Nodes[node.ID], childPending(child), 1)
	if err != nil {
		return child, err
	}
	p.Scheduling.Nodes[node.ID] = pending
	if err := tx.Put("master-task-children", childKey(p.ID, number), child); err != nil {
		return child, err
	}
	if err := tx.Put("master-task-outstanding", childKey(p.ID, number), number); err != nil {
		return child, err
	}
	var weight int64
	loadKey := key(p.ID) + "/" + node.ID
	if _, err := tx.Get("master-task-load", loadKey, &weight); err != nil {
		return child, err
	}
	// Include a small per-object cost so zero-byte and tiny objects distribute.
	cost := bytes + int64(len(objects))*64*1024
	if cost < bytes || weight > math.MaxInt64-cost {
		return child, errors.New("task load overflow")
	}
	if err := tx.Put("master-task-load", loadKey, weight+cost); err != nil {
		return child, err
	}
	p.DispatchedThrough = number
	p.DispatchedObjects += int64(len(objects))
	p.DispatchedBytes += bytes
	p.ScannedObjects = max(p.ScannedObjects, p.DispatchedObjects)
	p.ScannedBytes = max(p.ScannedBytes, p.DispatchedBytes)
	p.ScannedBatches = max(p.ScannedBatches, number)
	p.ChildCount++
	return child, nil
}

func (r *Repository) SetBandwidth(id int64, mbps float64) (Parent, error) {
	return r.SetLimits(id, &mbps, nil, false)
}

// SetLimits commits one revision for all task limits. Absent fields preserve
// their stored value, so bandwidth-only clients cannot discard request limits.
func (r *Repository) SetLimits(id int64, mbps *float64, limits *execution.RequestLimits, replaceRequests bool) (Parent, error) {
	return r.UpdateLimits(id, mbps, limits, replaceRequests, nil)
}

func (r *Repository) UpdateLimits(id int64, mbps *float64, limits *execution.RequestLimits, replaceRequests bool, patch *RequestLimitsPatch) (p Parent, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if Ended(p.Status) || p.Deleted || p.Purged {
			return ErrState
		}
		before := p.Spec
		if mbps != nil {
			p.Spec.BandwidthMbps = *mbps
		}
		if replaceRequests {
			p.Spec.RequestLimits = nil
			if limits != nil {
				copy := *limits
				if copy.Scope == "" {
					copy.Scope = "CLUSTER"
				}
				p.Spec.RequestLimits = &copy
			}
		}
		if patch != nil {
			if replaceRequests {
				return errors.New("cannot replace and patch request limits together")
			}
			value := execution.RequestLimits{Scope: "CLUSTER"}
			if p.Spec.RequestLimits != nil {
				value = *p.Spec.RequestLimits
			}
			if patch.ListQPS != nil {
				value.ListQPS = *patch.ListQPS
			}
			if patch.HeadQPS != nil {
				value.HeadQPS = *patch.HeadQPS
			}
			if patch.PutQPS != nil {
				value.PutQPS = *patch.PutQPS
			}
			p.Spec.RequestLimits = &value
		}
		if err := validate(p.Spec); err != nil {
			return err
		}
		if p.Spec.BandwidthMbps > 0 && math.Floor(p.Spec.BandwidthMbps*125000) < float64(len(p.Spec.Nodes)) {
			return errors.New("bandwidth budget is too small for the selected Agents")
		}
		if !reflect.DeepEqual(before, p.Spec) {
			p.BudgetRevision++
		}
		return save(tx, p)
	})
	return
}
func (r *Repository) Recycle(id int64, deleted bool) (p Parent, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if !Ended(p.Status) || p.Purged {
			return ErrState
		}
		p.Deleted = deleted
		return save(tx, p)
	})
	return
}

// Purge retains a compact tombstone to reject late events and creation replays.
// It removes only task metadata; source and destination data are never deleted.
func (r *Repository) Purge(id int64) (p Parent, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if !p.Deleted || !Ended(p.Status) {
			return ErrState
		}
		p.Purged = true
		for _, bucket := range []string{"master-task-children", "master-task-outstanding", "master-task-load", "master-task-exceptions", "master-task-exceptions-hidden", "master-task-legacy-children", "master-task-legacy-history", "master-task-legacy-order"} {
			for {
				var keys []string
				err := tx.Scan(bucket, key(id)+"/", "", 128, func(k string, _ json.RawMessage) error { keys = append(keys, k); return nil })
				if err != nil {
					return err
				}
				for _, k := range keys {
					if err := tx.Delete(bucket, k); err != nil {
						return err
					}
				}
				if len(keys) < 128 {
					break
				}
			}
		}
		return save(tx, p)
	})
	return
}
func (r *Repository) Children(id int64, after int, limit int) (result []Child, err error) {
	if limit < 1 || limit > 256 {
		return nil, errors.New("page size must be between 1 and 256")
	}
	err = r.store.View(func(tx *state.Tx) error {
		return tx.Scan("master-task-children", key(id)+"/", childKey(id, after), limit, func(_ string, raw json.RawMessage) error {
			var child Child
			if err := json.Unmarshal(raw, &child); err != nil {
				return err
			}
			result = append(result, child)
			return nil
		})
	})
	return
}

func (r *Repository) Outstanding(id int64, after int, limit int) (result []Child, err error) {
	if limit < 1 || limit > 256 {
		return nil, errors.New("page size must be between 1 and 256")
	}
	err = r.store.View(func(tx *state.Tx) error {
		return tx.Scan("master-task-outstanding", key(id)+"/", childKey(id, after), limit, func(k string, raw json.RawMessage) error {
			var c Child
			found, err := tx.Get("master-task-children", k, &c)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			result = append(result, c)
			return nil
		})
	})
	return
}

// Observation pairs a requested child with the Agent's durable result.
type Observation struct {
	Number int
	Record execution.Record
}

func (r *Repository) Observe(id int64, generation uint64, number int, record execution.Record) error {
	return r.ObserveBatch(id, generation, []Observation{{Number: number, Record: record}})
}

// ObserveBatch commits one bounded polling page atomically. Results remain in
// the Agent ledger until collected, so a failed transaction can be retried
// without losing object outcomes. The parent counters and indexes are written
// once, rather than once for every child in the page.
func (r *Repository) ObserveBatch(id int64, generation uint64, observations []Observation) error {
	if len(observations) == 0 {
		return nil
	}
	if len(observations) > 256 {
		return errors.New("observation page exceeds 256 children")
	}
	return unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.Purged {
			return ErrState
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		changed := false
		for _, observation := range observations {
			updated, err := observeChild(tx, &p, observation.Number, observation.Record)
			if err != nil {
				return err
			}
			changed = changed || updated
		}
		if !changed {
			return errUnchanged
		}
		return save(tx, p)
	}))
}

func observeChild(tx *state.Tx, p *Parent, number int, record execution.Record) (bool, error) {
	var child Child
	found, err := tx.Get("master-task-children", childKey(p.ID, number), &child)
	if err != nil {
		return false, err
	}
	if !found {
		return false, ErrNotFound
	}
	if record.Job.ChildID != child.Job.ChildID || record.Job.Generation != p.Generation || digest(record.Job) != digest(child.Job) {
		return false, ErrConflict
	}
	if child.Record != nil && (record.Attempt < child.Record.Attempt || (record.Attempt == child.Record.Attempt && record.UpdatedAt.Before(child.Record.UpdatedAt))) {
		return false, nil
	}
	if err := record.ValidateResolvedSources(p.Spec.InventoryID != "", child.Record); err != nil {
		return false, ErrConflict
	}
	if child.Accepted && child.Record != nil && reflect.DeepEqual(*child.Record, record) {
		return false, nil
	}
	for _, result := range record.Results {
		if result.ResolvedSource != nil {
			if err := state.RequireSchema(tx, "master", state.SchemaResolvedSources); err != nil {
				return false, err
			}
			break
		}
	}
	old := execution.Counters{}
	if child.Record != nil {
		old = child.Record.Counters
		countChild(p, child.Record.Status, -1)
	}
	countChild(p, record.Status, 1)
	addCounters(&p.Counters, old, -1)
	addCounters(&p.Counters, record.Counters, 1)
	previous := child
	child.Accepted = true
	child.Record = &record
	if err := changePending(p, previous, child); err != nil {
		return false, err
	}
	if err := syncExceptionIndex(tx, *p, child, record); err != nil {
		return false, err
	}
	if err := tx.Put("master-task-children", childKey(p.ID, number), child); err != nil {
		return false, err
	}
	if record.Status == execution.Completed || record.Status == execution.Failed || record.Status == execution.PartialFailed {
		if err := tx.Delete("master-task-outstanding", childKey(p.ID, number)); err != nil {
			return false, err
		}
	}
	return true, nil
}
func countChild(p *Parent, status execution.Status, delta int) {
	switch status {
	case execution.Completed:
		p.FinishedChildren += delta
	case execution.Failed, execution.PartialFailed:
		p.FinishedChildren += delta
		p.FailedChildren += delta
	}
}
func addCounters(to *execution.Counters, value execution.Counters, sign int64) {
	to.Total += sign * value.Total
	to.TotalBytes += sign * value.TotalBytes
	to.Succeeded += sign * value.Succeeded
	to.SucceededBytes += sign * value.SucceededBytes
	to.Skipped += sign * value.Skipped
	to.SkippedBytes += sign * value.SkippedBytes
	to.Failed += sign * value.Failed
	to.FailedBytes += sign * value.FailedBytes
}

func (r *Repository) ScanProgress(id int64, generation uint64, objects, bytes int64, batches int, complete bool, scanErr string) error {
	return unchangedOK(r.store.Update(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.Status != "RUNNING" {
			return ErrState
		}
		if objects < 0 || bytes < 0 || batches < 0 {
			return ErrConflict
		}
		if p.ScannedObjects >= objects && p.ScannedBytes >= bytes && p.ScannedBatches >= batches && p.ScanComplete == complete && p.Error == scanErr {
			return errUnchanged
		}
		p.ScannedObjects = max(p.ScannedObjects, objects)
		p.ScannedBytes = max(p.ScannedBytes, bytes)
		p.ScannedBatches = max(p.ScannedBatches, batches)
		p.ScanComplete = complete
		p.Error = scanErr
		return save(tx, p)
	}))
}
func (r *Repository) Stop(id int64) (p Parent, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if Ended(p.Status) {
			return nil
		}
		if p.Status != "STOPPING" {
			p.BudgetRevision++
		}
		p.Status = "STOPPING"
		return save(tx, p)
	})
	return
}
func (r *Repository) Stopped(id int64, generation uint64) error {
	return r.store.Update(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.Status != "STOPPING" && p.Status != "STOPPED" {
			return ErrState
		}
		p.Status = "STOPPED"
		now := time.Now().UTC()
		p.FinishedAt = &now
		return save(tx, p)
	})
}

// Resume retains immutable assignments and completed object ledgers. Agents must
// acknowledge the new parent generation before any child is dispatched again.
func (r *Repository) Resume(id int64) (p Parent, err error) {
	err = r.store.Update(func(tx *state.Tx) error {
		p, err = get(tx, id)
		if err != nil {
			return err
		}
		if p.Deleted || p.Status == "COMPLETED" || !Ended(p.Status) {
			return ErrState
		}
		if p.Legacy != nil && p.Legacy.NeedsReconciliation {
			return ErrLegacyRecovery
		}
		p.Generation++
		p.BudgetRevision++
		p.Status = "RESUMING"
		p.FinishedAt = nil
		p.Error = ""
		p.Scheduling = nil // Failed children reenter the pending index below.
		after := ""
		for {
			var page []Child
			var last string
			err := tx.Scan("master-task-children", key(id)+"/", after, 128, func(k string, raw json.RawMessage) error {
				var c Child
				if err := json.Unmarshal(raw, &c); err != nil {
					return err
				}
				page = append(page, c)
				last = k
				return nil
			})
			if err != nil {
				return err
			}
			for _, c := range page {
				c.Job.Generation = p.Generation
				c.Accepted = false
				if c.Record != nil && c.Record.Status != execution.Completed {
					countChild(&p, c.Record.Status, -1)
					c.Record.Status = execution.Waiting
				}
				if c.Record == nil || c.Record.Status != execution.Completed {
					if err := tx.Put("master-task-outstanding", childKey(id, c.Job.Number), c.Job.Number); err != nil {
						return err
					}
				}
				if err := tx.Put("master-task-children", childKey(id, c.Job.Number), c); err != nil {
					return err
				}
			}
			if len(page) < 128 {
				break
			}
			after = last
		}
		return save(tx, p)
	})
	return
}
func (r *Repository) Resumed(id int64, generation uint64) error {
	return r.store.Update(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.Status != "RESUMING" {
			return ErrState
		}
		p.Status = "RUNNING"
		return save(tx, p)
	})
}

func (r *Repository) FinishIfReady(id int64, generation uint64) (bool, error) {
	finished := false
	err := r.store.Update(func(tx *state.Tx) error {
		p, err := get(tx, id)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		if p.Status != "RUNNING" || (!p.ScanComplete && p.Error == "") || p.DispatchedThrough != p.ScannedBatches || p.FinishedChildren != p.ChildCount {
			return errUnchanged
		}
		failed := p.Error != "" || p.FailedChildren > 0
		p.Status = "COMPLETED"
		if failed {
			p.Status = "PARTIAL_FAILED"
			if p.Counters.Succeeded+p.Counters.Skipped == 0 {
				p.Status = "FAILED"
			}
		}
		now := time.Now().UTC()
		p.FinishedAt = &now
		finished = true
		return save(tx, p)
	})
	return finished, unchangedOK(err)
}
