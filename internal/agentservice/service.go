// Package agentservice exposes typed execution services over the outbound Agent
// channel. All data-plane I/O happens here, never in the Master HTTP process.
package agentservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/inventory"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type Service struct {
	store        *state.Store
	Updater      softwareupdate.Controller
	Comparisons  *comparison.Manager
	Inventory    *inventory.Repository
	Rates        *TaskRates
	Nodes        *nodecontrol.Controller
	Configs      *storageconfig.Agent
	Factory      *storageconfig.Factory
	Jobs         *execution.Repository
	Engine       *execution.Engine
	Scanner      *discovery.Scanner
	Limits       *ratelimit.Limiter
	Shared       *SharedVerifier
	Leases       *Leases
	Temporary    *LocalTempRegistry
	TempRecovery TempRecoveryReport
	MPURecovery  S3RecoveryReport
	preflight    *preflightProbes
	ctx          context.Context
	cancel       context.CancelFunc
	wake         chan struct{}
	workers      sync.WaitGroup
	nodeConfigMu sync.Mutex
	softwareMu   sync.RWMutex
}
type Options struct {
	Updater        softwareupdate.Controller
	NodeControl    nodecontrol.Options
	Execution      execution.Options
	SpoolDirectory string
}

func New(store *state.Store, options Options) (*Service, error) {
	if err := store.ClaimRole("agent"); err != nil {
		return nil, err
	}
	preflight := &preflightProbes{store: store}
	if err := preflight.fencePending(); err != nil {
		return nil, err
	}
	configs := storageconfig.NewAgent(store)
	uploadMemory, err := transferstore.NewUploadMemoryBudget(transferstore.DefaultUploadMemoryBytes)
	if err != nil {
		return nil, err
	}
	factory := &storageconfig.Factory{Source: configs, SpoolDirectory: options.SpoolDirectory, UploadMemory: uploadMemory}
	limits, err := ratelimit.New(store, ratelimit.Options{})
	if err != nil {
		return nil, err
	}
	nodes, err := nodecontrol.New(store, limits, options.NodeControl)
	if err != nil {
		limits.Close()
		return nil, err
	}
	configureNodeFactory(factory, nodes)
	rates := NewTaskRates()
	options.Execution.DecorateReader = rates.Decorator(nodes.Decorator(limits.Decorator))
	jobs, err := execution.NewRepository(store, execution.RepositoryOptions{})
	if err != nil {
		nodes.Close()
		limits.Close()
		return nil, err
	}
	temporary, err := NewLocalTempRegistry(store)
	if err != nil {
		nodes.Close()
		limits.Close()
		return nil, err
	}
	recovery, err := temporary.Recover(context.Background())
	if err != nil {
		temporary.Close()
		nodes.Close()
		limits.Close()
		return nil, err
	}
	mpu, err := NewS3RecoveryRegistry(store, factory, temporary)
	if err != nil {
		temporary.Close()
		nodes.Close()
		limits.Close()
		return nil, err
	}
	mpuRecovery, err := mpu.Recover(context.Background())
	if err != nil {
		temporary.Close()
		nodes.Close()
		limits.Close()
		return nil, err
	}
	shared := NewSharedVerifier()
	leases, err := NewLeases(store)
	if err != nil {
		temporary.Close()
		nodes.Close()
		limits.Close()
		return nil, err
	}
	options.Execution.ParentContext = leases.ParentContext
	if concurrency, saved := nodes.SavedConcurrency(); saved {
		options.Execution.MaxConcurrentJobs = concurrency.MaxConcurrentChildren
		options.Execution.WorkersPerJob = concurrency.ObjectsPerChild
	}
	engine, err := execution.NewEngine(jobs, mpu.Factory(temporary.Factory(shared.Factory(factory))), options.Execution)
	if err != nil {
		temporary.Close()
		nodes.Close()
		limits.Close()
		return nil, err
	}
	scanner, err := discovery.New(store, factory, discovery.Options{ParentContext: leases.ParentContext})
	if err != nil {
		_ = engine.Close(context.Background())
		temporary.Close()
		nodes.Close()
		limits.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{store: store, Inventory: inventory.New(store), Rates: rates, Nodes: nodes, Configs: configs, Factory: factory, Jobs: jobs, Engine: engine, Scanner: scanner, Limits: limits, Shared: shared, Temporary: temporary, TempRecovery: recovery, MPURecovery: mpuRecovery, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	s.Leases = leases
	leases.quiesce = func(ctx context.Context, parentID int64) error {
		return errors.Join(engine.QuiesceParent(ctx, parentID), scanner.QuiesceParent(ctx, parentID))
	}
	s.Updater = options.Updater
	s.preflight = preflight
	comparisons, err := comparison.New(store, factory, func(ctx context.Context, input io.ReadCloser, size int64) (io.ReadCloser, error) {
		return nodes.Decorator(limits.Decorator)(ctx, execution.Job{}, execution.Item{Source: transferstore.Object{Size: size}}, input)
	})
	if err != nil {
		cancel()
		_ = scanner.Close()
		_ = engine.Close(context.Background())
		temporary.Close()
		nodes.Close()
		limits.Close()
		return nil, err
	}
	s.Comparisons = comparisons
	s.workers.Add(1)
	go s.schedule()
	go preflight.run(ctx)
	return s, nil
}
func (s *Service) Close(ctx context.Context) error {
	s.Leases.Close()
	s.cancel()
	if s.Comparisons != nil {
		s.Comparisons.Close()
	}
	s.workers.Wait()
	scanErr := s.Scanner.Close()
	err := s.Engine.Close(ctx)
	nodeErr := s.Nodes.Close()
	s.Limits.Close()
	s.Temporary.Close()
	if s.Factory.HTTPClient != nil {
		s.Factory.HTTPClient.CloseIdleConnections()
	}
	return errors.Join(scanErr, err, nodeErr)
}
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) schedule() {
	defer s.workers.Done()
	timer := time.NewTicker(200 * time.Millisecond)
	defer timer.Stop()
	var after string
	capacityChanged := s.Engine.CapacityChanged()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-capacityChanged:
		case <-timer.C:
		}
		// Subscribe before admission; an attempt may finish while this batch is
		// still being started. Replacing this channel after admission loses it.
		capacityChanged = s.Engine.CapacityChanged()
		s.softwareMu.RLock()
		if s.softwareUpdating() {
			s.softwareMu.RUnlock()
			continue
		}
		active, capacity := s.Engine.Concurrency()
		if active >= capacity {
			s.softwareMu.RUnlock()
			continue
		}
		jobs, err := s.Jobs.Pending(s.ctx, after, 16)
		if err != nil {
			s.softwareMu.RUnlock()
			continue
		}
		if len(jobs) < 16 {
			after = ""
		} else {
			after = jobs[len(jobs)-1].Job.ChildID
		}
		for _, record := range jobs {
			if s.Leases.Admission(record.Job.ParentID, record.Job.Generation) != nil {
				continue
			}
			if !s.Shared.Allows(record.Job) {
				continue
			}
			if s.ctx.Err() != nil {
				s.softwareMu.RUnlock()
				return
			}
			if record.Status == execution.Recoverable {
				_ = s.Engine.TryRecover(s.ctx, record.Job.ChildID, record.Job.Generation)
			} else {
				_ = s.Engine.TryStart(s.ctx, record.Job.ChildID, record.Job.Generation)
			}
		}
		s.softwareMu.RUnlock()
		if len(jobs) == 16 {
			active, capacity := s.Engine.Concurrency()
			if active < capacity {
				s.signal()
			}
		}
	}
}

// These operations commit their own immutable IDs, configuration revisions or
// parent fences. They can reconcile a lost reply without a second command ledger.
func (s *Service) ReplaySafe(operation string) bool {
	if operation == "software_update" || operation == "backup_prune" {
		return true
	}
	switch operation {
	case "lease_prepare", "lease_grant", "lease_retire", "scan_takeover_seed", "scan_takeover_finalize":
		return true
	case "comparison_start", "comparison_cancel", "comparison_delete":
		return true
	case "inventory_begin", "inventory_batch", "inventory_seal", "storage_apply", "child_accept", "child_recover", "parent_stop", "parent_resume", "parent_delete", "parent_cleanup", "scan_start", "scan_ack", "scan_release", "scan_stop", "scan_resume", "bandwidth_parent", "bandwidth_node", "local_prepare", "shared_probe_ensure", "shared_probe_verify", "shared_probe_cleanup", "preflight_probe":
		return true
	}
	return false
}

func decode(data json.RawMessage, into any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func response(value any, err error) (agenttransport.Response, error) {
	if err == nil {
		if scan, ok := value.(discovery.State); ok {
			scan.ReleaseProtocol = 1
			value = scan
		}
		data, e := json.Marshal(value)
		return agenttransport.Response{Status: 200, Payload: data}, e
	}
	status, code := 500, "operation_failed"
	switch {
	case errors.Is(err, discovery.ErrBatchReleased):
		status, code = 409, "scan_batch_released"
	case errors.Is(err, inventory.ErrNotFound), errors.Is(err, execution.ErrNotFound), errors.Is(err, discovery.ErrNotFound), errors.Is(err, storageconfig.ErrNotFound), errors.Is(err, os.ErrNotExist), errors.Is(err, transferstore.ErrNotFound), errors.Is(err, ratelimit.ErrMissing):
		status = 404
	case errors.Is(err, discovery.ErrEpoch), errors.Is(err, discovery.ErrSeeding), errors.Is(err, discovery.ErrRunning), errors.Is(err, execution.ErrAssignment), errors.Is(err, execution.ErrRunning), errors.Is(err, ErrLeaseRequired), errors.Is(err, ErrLeaseExpired), errors.Is(err, ErrLeaseRetired), errors.Is(err, ErrLeaseConflict), errors.Is(err, inventory.ErrConflict), errors.Is(err, inventory.ErrUnsealed), errors.Is(err, execution.ErrGeneration), errors.Is(err, execution.ErrConflict), errors.Is(err, execution.ErrStopped), errors.Is(err, execution.ErrDeleted), errors.Is(err, execution.ErrNotRecoverable), errors.Is(err, storageconfig.ErrStaleRevision), errors.Is(err, storageconfig.ErrRevisionConflict), errors.Is(err, discovery.ErrGeneration), errors.Is(err, discovery.ErrConflict), errors.Is(err, discovery.ErrStopped), errors.Is(err, discovery.ErrDeleted), errors.Is(err, ratelimit.ErrStale), errors.Is(err, ratelimit.ErrConflict), errors.Is(err, ErrSharedVerificationRequired), errors.Is(err, ErrSharedVerificationStale):
		status = 409
	case errors.Is(err, inventory.ErrCapacity), errors.Is(err, execution.ErrCapacity), errors.Is(err, discovery.ErrCapacity):
		status = 429
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status = 408
	case errors.Is(err, ErrInvalidLease), errors.Is(err, inventory.ErrInvalid), errors.Is(err, storageconfig.ErrInvalid), errors.Is(err, transferstore.ErrInvalidKey), errors.Is(err, discovery.ErrInvalid):
		status = 400
	}
	data, _ := json.Marshal(map[string]any{"code": code, "message": err.Error()})
	return agenttransport.Response{Status: status, Payload: data}, nil
}
func badRequest(err error) (agenttransport.Response, error) {
	data, _ := json.Marshal(map[string]string{"code": "invalid_request", "message": err.Error()})
	return agenttransport.Response{Status: 400, Payload: data}, nil
}

type ChildAcceptRequest struct {
	execution.Job
	InventorySource bool              `json:"inventorySource,omitempty"`
	Seed            *execution.Record `json:"seed,omitempty"`
}

type ParentRequest struct {
	InventorySource bool   `json:"inventorySource,omitempty"`
	ParentID        int64  `json:"parentId"`
	Generation      uint64 `json:"generation"`
	RetryFailed     bool   `json:"retryFailed,omitempty"`
}
type ChildrenRequest struct {
	ParentID int64  `json:"parentId"`
	After    string `json:"after"`
	Limit    int    `json:"limit"`
}
type StatRequest struct {
	Endpoint execution.Endpoint `json:"endpoint"`
	Key      string             `json:"key"`
}
type PathRequest struct {
	Path string `json:"path"`
}
type ScanRequest struct {
	ParentID   int64  `json:"parentId"`
	Generation uint64 `json:"generation"`
	Number     int64  `json:"number,omitempty"`
	Epoch      uint64 `json:"epoch,omitempty"`
}

func (s *Service) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	if c.Operation == "children_get" {
		return s.childRecords(ctx, c)
	}
	if c.Operation == "scan_batches" {
		return s.scanBatches(ctx, c)
	}
	if result, handled, err := s.softwareCommand(ctx, c); handled {
		return result, err
	}
	if softwareupdate.StartsWork(c.Operation) {
		s.softwareMu.RLock()
		defer s.softwareMu.RUnlock()
		if s.softwareUpdating() {
			return agenttransport.Response{Status: 409, Payload: json.RawMessage(`{"code":"updating","message":"Agent update is in progress"}`)}, nil
		}
	}
	if response, handled, err := s.comparisonCommand(ctx, c); handled {
		return response, err
	}
	if response, handled, err := s.nodeCommand(ctx, c); handled {
		return response, err
	}
	if s.ReplaySafe(c.Operation) && !c.Mutating {
		return badRequest(errors.New("operation requires a mutating command"))
	}
	if result, handled, err := s.scanTakeoverCommand(ctx, c); handled {
		return result, err
	}
	switch c.Operation {
	case "lease_prepare":
		var request LeasePrepareRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		value, err := s.Leases.Prepare(ctx, request)
		return response(value, err)
	case "lease_grant":
		var request LeaseGrantRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		err := s.Leases.Grant(ctx, request)
		if err == nil {
			s.signal()
		}
		return response(map[string]any{"parentId": request.ParentID, "sequence": request.Sequence}, err)
	case "lease_retire":
		var request LeaseRetireRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		err := s.Leases.Retire(ctx, request.ParentID)
		return response(map[string]any{"parentId": request.ParentID, "retired": err == nil}, err)
	case "child_recover":
		var request ChildRecoverRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		value, err := s.recoverChild(ctx, request)
		return response(value, err)
	case "preflight_probe":
		var request PreflightProbeRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		err := s.preflight.execute(ctx, request)
		return response(map[string]any{"verified": err == nil}, err)
	case "task_metrics":
		var request struct {
			ParentID   int64  `json:"parentId"`
			Generation uint64 `json:"generation"`
		}
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		if request.ParentID < 1 || request.Generation == 0 {
			return badRequest(errors.New("parent and generation are required"))
		}
		snapshot := s.Rates.Snapshot(request.ParentID, request.Generation)
		snapshot.ProgressSupported = true
		snapshot.Progress = s.Engine.Progress(request.ParentID, request.Generation)
		return response(snapshot, nil)
	case "task_progress":
		var request struct {
			ParentID   int64  `json:"parentId"`
			Generation uint64 `json:"generation"`
			ChildID    string `json:"childId"`
		}
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		if request.ParentID < 1 || request.Generation == 0 || request.ChildID == "" || len(request.ChildID) > 256 {
			return badRequest(errors.New("parent, generation and child are required"))
		}
		child, items, found := s.Engine.ProgressObjects(request.ParentID, request.Generation, request.ChildID)
		var summary *execution.ChildProgress
		if found {
			summary = &child
		}
		if items == nil {
			items = []execution.ObjectProgress{}
		}
		return response(map[string]any{"supported": true, "child": summary, "items": items}, nil)
	case "shared_probe_ensure", "shared_probe_verify", "shared_probe_cleanup":
		var request struct {
			ParentID   int64                     `json:"parentId"`
			Generation uint64                    `json:"generation"`
			Root       string                    `json:"root"`
			Probe      transferstore.SharedProbe `json:"probe"`
		}
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		if request.ParentID < 1 || request.Generation == 0 || !filepath.IsAbs(request.Root) {
			return badRequest(errors.New("shared probe requires parent, generation and absolute root"))
		}
		var err error
		switch c.Operation {
		case "shared_probe_ensure":
			err = transferstore.EnsureSharedProbe(ctx, request.Root, request.Probe)
		case "shared_probe_verify":
			err = s.Shared.Verify(ctx, request.ParentID, request.Generation, request.Root, request.Probe)
			if err == nil {
				s.signal()
			}
		case "shared_probe_cleanup":
			err = transferstore.CleanupSharedProbe(ctx, request.Root, request.Probe)
		}
		return response(map[string]any{"verified": err == nil}, err)
	case "report_page":
		var request ReportRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		page, err := s.Report(ctx, request)
		return response(page, err)
	case "bandwidth_parent_status":
		var request struct {
			ParentID int64 `json:"parentId"`
		}
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		policy, err := s.Limits.Parent(request.ParentID)
		return response(policy, err)
	case "bandwidth_parent":
		var policy ratelimit.ParentLimit
		if err := decode(c.Payload, &policy); err != nil {
			return badRequest(err)
		}
		err := s.Limits.ApplyParent(ctx, policy)
		return response(policy, err)
	case "bandwidth_node":
		var policy ratelimit.NodeLimit
		if err := decode(c.Payload, &policy); err != nil {
			return badRequest(err)
		}
		err := s.Limits.ApplyNode(ctx, policy)
		return response(policy, err)
	case "inventory_begin", "inventory_batch", "inventory_seal":
		return s.inventoryCommand(ctx, c)
	case "scan_start":
		var spec discovery.Spec
		if err := decode(c.Payload, &spec); err != nil {
			return badRequest(err)
		}
		value, err := s.Scanner.Start(ctx, spec)
		return response(value, err)
	case "scan_status", "scan_batch", "scan_ack", "scan_release", "scan_stop", "scan_resume":
		var request ScanRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		if c.Operation == "scan_batch" {
			batch, found, err := s.Scanner.Batch(ctx, request.ParentID, request.Generation, request.Number, request.Epoch)
			return response(map[string]any{"batch": batch, "found": found}, err)
		}
		var value discovery.State
		var err error
		switch c.Operation {
		case "scan_status":
			value, err = s.Scanner.StatusEpoch(ctx, request.ParentID, request.Generation, request.Epoch)
		case "scan_ack":
			value, err = s.Scanner.Ack(ctx, request.ParentID, request.Generation, request.Number, request.Epoch)
		case "scan_release":
			value, err = s.Scanner.Release(ctx, request.ParentID, request.Generation, request.Number, request.Epoch)
		case "scan_stop":
			value, err = s.Scanner.Stop(ctx, request.ParentID, request.Generation, request.Epoch)
		case "scan_resume":
			value, err = s.Scanner.Resume(ctx, request.ParentID, request.Generation, request.Epoch)
		}
		return response(value, err)
	case "storage_apply":
		var snapshot storageconfig.Snapshot
		if err := decode(c.Payload, &snapshot); err != nil {
			return badRequest(err)
		}
		_, err := s.Configs.ApplySnapshot(snapshot)
		return response(map[string]any{"revision": snapshot.Revision}, err)
	case "storage_revision":
		snapshot, err := s.Configs.Snapshot()
		return response(map[string]any{"revision": snapshot.Revision}, err)
	case "storage_buckets":
		var config storageconfig.Config
		if err := decode(c.Payload, &config); err != nil {
			return badRequest(err)
		}
		settings, err := config.S3Config("validation")
		if err != nil {
			return response(nil, err)
		}
		settings.HTTPClient = s.Factory.HTTPClientForEndpoint(ctx, execution.Endpoint{Ref: config.Alias}, config)
		buckets, err := transferstore.ListBuckets(ctx, settings)
		return response(map[string]any{"buckets": buckets}, err)
	case "object_stat":
		var request StatRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		store, err := s.Factory.OpenEndpoint(ctx, request.Endpoint)
		if err != nil {
			return response(nil, err)
		}
		defer store.Close()
		object, err := store.Stat(ctx, request.Key)
		return response(object, err)
	case "local_path", "local_prepare":
		var request PathRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		if !filepath.IsAbs(request.Path) {
			return badRequest(errors.New("path must be absolute on this Agent"))
		}
		if c.Operation == "local_prepare" {
			if err := os.MkdirAll(request.Path, 0750); err != nil {
				return response(nil, err)
			}
		}
		info, err := os.Stat(request.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && c.Operation == "local_path" {
				return response(map[string]any{"path": request.Path, "pathType": "MISSING", "exists": false}, nil)
			}
			return response(nil, err)
		}
		kind := "FILE"
		if info.IsDir() {
			kind = "DIRECTORY"
		} else if !info.Mode().IsRegular() {
			return badRequest(errors.New("path must be a regular file or directory"))
		}
		return response(map[string]any{"path": request.Path, "pathType": kind, "size": info.Size(), "exists": true}, nil)
	case "child_accept":
		var request ChildAcceptRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		job := request.Job
		if err := s.Leases.Admission(job.ParentID, job.Generation); err != nil {
			return response(nil, err)
		}
		if !s.Shared.Allows(job) {
			return response(nil, ErrSharedVerificationRequired)
		}
		record, err := s.Engine.AcceptSeed(ctx, job, request.Seed, request.InventorySource)
		if err == nil {
			s.signal()
		}
		return response(map[string]any{"childId": record.Job.ChildID, "generation": record.Job.Generation, "status": record.Status}, err)
	case "child_get":
		var request struct {
			ChildID string `json:"childId"`
		}
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		record, err := s.Engine.Get(ctx, request.ChildID)
		return response(record, err)
	case "children":
		var request ChildrenRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		if request.Limit == 0 {
			request.Limit = 32
		}
		records, err := s.Jobs.List(ctx, request.ParentID, request.After, request.Limit)
		return response(records, err)
	case "parent_cleanup":
		var request ParentRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		progress, err := s.cleanupParent(ctx, request)
		return response(progress, err)
	case "parent_stop", "parent_resume", "parent_delete":
		var request ParentRequest
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		var err error
		switch c.Operation {
		case "parent_stop":
			err = s.Engine.StopParent(ctx, request.ParentID, request.Generation)
		case "parent_resume":
			if err := s.Leases.Admission(request.ParentID, request.Generation); err != nil {
				return response(nil, err)
			}
			err = s.Engine.ResumeParent(ctx, request.ParentID, request.Generation, request.RetryFailed, request.InventorySource)
		case "parent_delete":
			err = s.Engine.DeleteParent(ctx, request.ParentID, request.Generation)
			if err == nil {
				err = s.Scanner.DeleteParent(ctx, request.ParentID, request.Generation)
			}
		}
		if err == nil {
			if c.Operation != "parent_resume" {
				s.Shared.Forget(request.ParentID, 0)
			}
			s.signal()
		}
		return response(map[string]any{"parentId": request.ParentID, "generation": request.Generation}, err)
	default:
		return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
	}
}
