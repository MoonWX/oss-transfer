package nodecontrol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type sample struct {
	second int64
	values [4]int64
}

type Controller struct {
	db                    *state.Store
	bytes                 ByteLimiter
	options               Options
	configured            map[string]float64
	mu                    sync.Mutex
	data                  persisted
	processID             string
	next                  map[string]time.Time
	changed               chan struct{}
	samples               [6]sample
	consecutive           int
	last503, nextRecovery time.Time
	fault                 error
	closed                bool
	stop                  chan struct{}
	done                  chan struct{}
}

func New(db *state.Store, bytes ByteLimiter, options Options) (*Controller, error) {
	if db == nil || bytes == nil {
		return nil, ErrInvalid
	}
	if !options.UseZeroDefaults {
		if options.ListQPS == 0 {
			options.ListQPS = 100
		}
		if options.PutQPS == 0 {
			options.PutQPS = 1400
		}
		if options.HeadQPS == 0 {
			options.HeadQPS = 2800
		}
		if options.BandwidthBitsPerSecond == 0 {
			options.BandwidthBitsPerSecond = 2_000_000_000
		}
	}
	if options.Threshold == 0 {
		options.Threshold = 20
	}
	if options.ReduceFactor == 0 {
		options.ReduceFactor = 0.9
	}
	if options.IncreaseFactor == 0 {
		options.IncreaseFactor = 1.2
	}
	if options.MinQPS == 0 {
		options.MinQPS = 200
	}
	if options.RecoveryInterval == 0 {
		options.RecoveryInterval = 10 * time.Second
	}
	if options.Threshold < 1 || options.Threshold > 1_000_000 || options.ReduceFactor <= 0 || options.ReduceFactor >= 1 || !finite(options.ReduceFactor) || options.IncreaseFactor <= 1 || !finite(options.IncreaseFactor) || options.MinQPS <= 0 || !finite(options.MinQPS) || options.RecoveryInterval <= 0 {
		return nil, ErrInvalid
	}
	configured := map[string]float64{"list": options.ListQPS, "put": options.PutQPS, "head": options.HeadQPS, "bandwidth": options.BandwidthBitsPerSecond}
	for kind, value := range configured {
		if !validRate(kind, value) {
			return nil, ErrInvalid
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	c := &Controller{db: db, bytes: bytes, options: options, configured: configured, processID: hex.EncodeToString(nonce[:]), next: map[string]time.Time{}, changed: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	err := db.Update(func(tx *state.Tx) error {
		found, err := tx.Get("node-control", "settings", &c.data)
		if err != nil {
			return err
		}
		if !found {
			c.data = persisted{Overrides: map[string]float64{}, Effective: map[string]float64{}, Last: map[string]Adjustment{}, Incidents: map[string]string{}, Roots: []string{}}
		}
		if c.data.Overrides == nil || c.data.Effective == nil || c.data.Last == nil || c.data.Incidents == nil {
			return errors.New("invalid saved node configuration")
		}
		for kind, value := range c.data.Overrides {
			if !validRate(kind, value) {
				return errors.New("invalid saved node rate")
			}
		}
		if _, err := validatedRoots(c.data.Roots); err != nil {
			return err
		}
		if value := c.data.Concurrency; value != nil {
			if err := ValidateConcurrency(value.MaxConcurrentChildren, value.ObjectsPerChild); err != nil {
				return errors.New("invalid saved node concurrency")
			}
		}
		for kind := range configured {
			target := c.target(c.data, kind)
			actual, ok := c.data.Effective[kind]
			if !ok {
				c.data.Effective[kind] = target
			} else if !validRate(kind, actual) || actual == 0 && target > 0 {
				return errors.New("invalid saved effective rate")
			}
			if target == 0 || actual > target {
				c.data.Effective[kind] = target
			}
		}
		return tx.Put("node-control", "settings", c.data)
	})
	if err != nil {
		return nil, err
	}
	// The byte limiter is the sole authoritative owner of node bandwidth. Its
	// existing persisted cap must never be overwritten with a startup default.
	if policy := bytes.Node(); policy.Revision == 0 {
		if err := bytes.ApplyNode(context.Background(), ratelimit.NodeLimit{Revision: 1, BandwidthBytesPerSecond: int64(c.target(c.data, "bandwidth") / 8)}); err != nil {
			return nil, err
		}
	}
	c.nextRecovery = time.Now().Add(options.RecoveryInterval)
	go c.recoverLoop()
	return c, nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func validRate(kind string, value float64) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return false
	}
	switch kind {
	case "list", "put", "head":
		return value <= 1_000_000 && (value == 0 || value >= 0.01)
	case "bandwidth":
		return value <= 1_000_000_000_000 && (value == 0 || value >= 8)
	}
	return false
}
func (c *Controller) target(data persisted, kind string) float64 {
	if value, ok := data.Overrides[kind]; ok {
		return value
	}
	return c.configured[kind]
}
func clone(data persisted) persisted {
	raw, _ := json.Marshal(data)
	var result persisted
	_ = json.Unmarshal(raw, &result)
	return result
}
func (c *Controller) wake() { close(c.changed); c.changed = make(chan struct{}) }
func (c *Controller) healthy() error {
	if c.closed {
		return ErrClosed
	}
	return c.fault
}

func (c *Controller) event(next *persisted, kind string, old, value, oldTarget, target float64, reason, incident string, trigger *RequestContext) Adjustment {
	next.Sequence++
	event := Adjustment{ID: fmt.Sprintf("%s:%d", c.processID, next.Sequence), ProcessID: c.processID, Sequence: next.Sequence, Type: kind, Value: value, PreviousValue: old, PreviousManualTarget: oldTarget, ManualTarget: target, Manual: reason == "MANUAL_SET", Timestamp: time.Now().UnixMilli(), ReasonCode: reason, IncidentID: incident, DetailsAvailable: true, IntervalMS: c.options.RecoveryInterval.Milliseconds(), IncreaseFactor: c.options.IncreaseFactor, TriggerRequest: trigger}
	if reason == "OSS_503" {
		event.Consecutive503Count = c.consecutive
		event.Consecutive503Threshold = c.options.Threshold
		event.Factor = c.options.ReduceFactor
		event.MinQPS = c.options.MinQPS
	}
	next.Last[kind] = event
	return event
}
func (c *Controller) commit(next persisted, events []Adjustment) error {
	err := c.db.Update(func(tx *state.Tx) error {
		for _, event := range events {
			if err := tx.Put("node-rate-events", event.ID, event); err != nil {
				return err
			}
			if err := tx.Put("node-rate-history", fmt.Sprintf("%020d", event.Sequence), event); err != nil {
				return err
			}
			if event.IncidentID != "" {
				if err := tx.Put("node-rate-incidents", event.IncidentID+fmt.Sprintf("/%020d", event.Sequence), event); err != nil {
					return err
				}
			}
		}
		return tx.Put("node-control", "settings", next)
	})
	if err != nil {
		return err
	}
	c.data = next
	c.wake()
	return nil
}

func (c *Controller) SetRate(ctx context.Context, kind string, value float64) error {
	if !validRate(kind, value) {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.healthy(); err != nil {
		return err
	}
	next := clone(c.data)
	previous, target := next.Effective[kind], c.target(next, kind)
	if kind == "bandwidth" {
		previous = float64(c.bytes.Node().BandwidthBytesPerSecond) * 8
	}
	if previous == value && target == value {
		return nil
	}
	if kind == "bandwidth" {
		policy := c.bytes.Node()
		if policy.Revision == math.MaxUint64 {
			return ErrInvalid
		}
		if err := c.bytes.ApplyNode(ctx, ratelimit.NodeLimit{Revision: policy.Revision + 1, BandwidthBytesPerSecond: int64(value / 8)}); err != nil {
			return err
		}
		value = float64(c.bytes.Node().BandwidthBytesPerSecond) * 8
	}
	next.Overrides[kind] = value
	next.Effective[kind] = value
	delete(next.Incidents, kind)
	event := c.event(&next, kind, previous, value, target, value, "MANUAL_SET", "", nil)
	if err := c.commit(next, []Adjustment{event}); err != nil {
		if kind == "bandwidth" {
			c.fault = fmt.Errorf("bandwidth applied but adjustment persistence failed: %w", err)
			return c.fault
		}
		return err
	}
	delete(c.next, kind)
	return nil
}

func validatedRoots(roots []string) ([]string, error) {
	if len(roots) > 100 {
		return nil, ErrInvalid
	}
	out := []string{}
	seen := map[string]bool{}
	for _, root := range roots {
		if !utf8.ValidString(root) || len(root) > 4096 || strings.ContainsAny(root, "\x00\r\n") || !filepath.IsAbs(root) {
			return nil, ErrInvalid
		}
		root = filepath.Clean(root)
		if !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}
	return out, nil
}
func (c *Controller) SetRoots(ctx context.Context, roots []string) error {
	values, err := validatedRoots(roots)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.healthy(); err != nil {
		return err
	}
	next := clone(c.data)
	next.Roots = values
	return c.commit(next, nil)
}
func (c *Controller) Config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config()
}

func kindIndex(kind string) int {
	switch kind {
	case "list":
		return 0
	case "put":
		return 1
	case "head":
		return 2
	case "bandwidth":
		return 3
	}
	return -1
}
func (c *Controller) addSample(index int, amount int64) {
	second := time.Now().Unix()
	entry := &c.samples[second%6]
	if entry.second != second {
		*entry = sample{second: second}
	}
	entry.values[index] += amount
}
func (c *Controller) ObserveBytes(amount int) {
	if amount <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addSample(3, int64(amount))
}

// Wait spends one actual HTTP attempt, not one logical file. No reservation is
// made for a sleeping caller, so cancelling it cannot leave future token debt.
func (c *Controller) Wait(ctx context.Context, kind string) error {
	index := kindIndex(kind)
	if index < 0 || index == 3 {
		return ErrInvalid
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if err := c.healthy(); err != nil {
			c.mu.Unlock()
			return err
		}
		now := time.Now()
		rate := c.data.Effective[kind]
		delay := c.next[kind].Sub(now)
		if rate == 0 || delay <= 0 {
			if rate > 0 {
				c.next[kind] = now.Add(time.Duration(float64(time.Second) / rate))
			}
			c.addSample(index, 1)
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (c *Controller) ObserveResponse(request RequestContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.healthy(); err != nil {
		return err
	}
	if request.StatusCode >= 200 && request.StatusCode < 400 {
		c.consecutive = 0
		return nil
	}
	if request.StatusCode != 503 {
		return nil
	}
	if c.consecutive < math.MaxInt {
		c.consecutive++
	}
	c.last503 = time.Now()
	c.nextRecovery = c.last503.Add(c.options.RecoveryInterval)
	if c.consecutive < c.options.Threshold {
		return nil
	}
	next := clone(c.data)
	events := []Adjustment{}
	incident := next.Incidents["put"]
	if incident == "" {
		incident = next.Incidents["head"]
	}
	if incident == "" {
		incident = fmt.Sprintf("%s-%d", c.processID, next.Sequence+1)
	}
	for _, kind := range []string{"put", "head"} {
		old, target := next.Effective[kind], c.target(next, kind)
		minimum := math.Min(target, c.options.MinQPS)
		if target <= 0 || old <= minimum {
			continue
		}
		value := math.Max(minimum, old*c.options.ReduceFactor)
		next.Effective[kind] = value
		next.Incidents[kind] = incident
		events = append(events, c.event(&next, kind, old, value, target, target, "OSS_503", incident, &request))
	}
	if len(events) == 0 {
		return nil
	}
	if err := c.commit(next, events); err != nil {
		c.fault = fmt.Errorf("automatic adjustment persistence failed: %w", err)
		return c.fault
	}
	return nil
}

func (c *Controller) Tick(now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.healthy(); err != nil {
		return err
	}
	if now.Before(c.nextRecovery) {
		return nil
	}
	next := clone(c.data)
	events := []Adjustment{}
	for _, kind := range []string{"put", "head"} {
		old, target := next.Effective[kind], c.target(next, kind)
		if target <= 0 || old >= target {
			continue
		}
		value := math.Min(target, old*c.options.IncreaseFactor)
		reason := "RECOVERY_STEP"
		if value == target {
			reason = "RECOVERED"
		}
		incident := next.Incidents[kind]
		events = append(events, c.event(&next, kind, old, value, target, target, reason, incident, nil))
		next.Effective[kind] = value
		if value == target {
			delete(next.Incidents, kind)
		}
	}
	if len(events) > 0 {
		if err := c.commit(next, events); err != nil {
			c.fault = fmt.Errorf("recovery adjustment persistence failed: %w", err)
			return c.fault
		}
	}
	c.nextRecovery = now.Add(c.options.RecoveryInterval)
	return nil
}
func (c *Controller) recoverLoop() {
	defer close(c.done)
	ticker := time.NewTicker(min(c.options.RecoveryInterval, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-ticker.C:
			_ = c.Tick(now)
		}
	}
}
func (c *Controller) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.stop)
		c.wake()
	}
	c.mu.Unlock()
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fault
}

func (c *Controller) Snapshot() (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.healthy(); err != nil {
		return nil, err
	}
	now := time.Now()
	result := map[string]any{}
	var totals [4]int64
	// Completed one-second buckets form a sliding 5-second measurement. Reading
	// the snapshot does not reset it, so multiple browser clients see one sample.
	for _, entry := range c.samples {
		if entry.second >= now.Unix()-5 && entry.second < now.Unix() {
			for i, value := range entry.values {
				totals[i] += value
			}
		}
	}
	for kind := range c.configured {
		current, target := c.data.Effective[kind], c.target(c.data, kind)
		unit := "Qps"
		actual := float64(totals[kindIndex(kind)]) / 5
		if kind == "bandwidth" {
			unit = "Bps"
			current = float64(c.bytes.Node().BandwidthBytesPerSecond) * 8
			target = current
			actual *= 8
		}
		source := "PERSISTED_MANUAL"
		if target == c.configured[kind] {
			source = "CONFIG"
		}
		status := "NORMAL"
		last, hasLast := c.data.Last[kind]
		if target == 0 {
			status = "UNLIMITED"
		} else if current < target {
			status = "REDUCED"
			if last.ReasonCode == "RECOVERY_STEP" {
				status = "RECOVERING"
			}
		} else if last.ReasonCode == "RECOVERED" {
			status = "RECOVERED"
		}
		rate := map[string]any{"limit" + unit: current, "manualLimit" + unit: target, "configuredLimit" + unit: c.configured[kind], "current" + unit: actual, "limitSource": source, "state": status}
		if hasLast {
			rate["lastAdjustment"] = last
		}
		result[kind] = rate
	}
	recent := []Adjustment{}
	err := c.db.View(func(tx *state.Tx) error {
		for sequence := c.data.Sequence; sequence > 0 && len(recent) < 100; sequence-- {
			var event Adjustment
			found, err := tx.Get("node-rate-history", fmt.Sprintf("%020d", sequence), &event)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			if event.ProcessID != c.processID {
				break
			}
			recent = append(recent, event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	auto := map[string]any{"processId": c.processID, "observedAt": now.UnixMilli(), "sequence": c.data.Sequence, "events": recent, "intervalMs": c.options.RecoveryInterval.Milliseconds(), "increaseFactor": c.options.IncreaseFactor, "reduceFactor": c.options.ReduceFactor, "minQps": c.options.MinQPS, "consecutive503Threshold": c.options.Threshold, "consecutive503Count": c.consecutive, "measurementIntervalMs": 5000, "lastMeasurementAt": now.Truncate(time.Second).UnixMilli()}
	if !c.last503.IsZero() {
		auto["last503At"] = c.last503.UnixMilli()
	}
	if len(c.data.Incidents) > 0 {
		auto["nextRecoveryAt"] = c.nextRecovery.UnixMilli()
	}
	result["autoAdjust"] = auto
	return result, nil
}

func (c *Controller) History(limit int) ([]Adjustment, error) {
	if limit < 1 || limit > 200 {
		return nil, ErrInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []Adjustment{}
	err := c.db.View(func(tx *state.Tx) error {
		for sequence := c.data.Sequence; sequence > 0 && len(out) < limit; sequence-- {
			var event Adjustment
			found, err := tx.Get("node-rate-history", fmt.Sprintf("%020d", sequence), &event)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			out = append(out, event)
		}
		return nil
	})
	return out, err
}
func (c *Controller) Detail(id, cursor string) (Detail, error) {
	if len(id) > 100 || len(cursor) > 160 {
		return Detail{}, ErrInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out Detail
	err := c.db.View(func(tx *state.Tx) error {
		found, err := tx.Get("node-rate-events", id, &out.Event)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		out.ProcessRestarted = out.Event.ProcessID != c.processID
		out.HistoryComplete = true
		out.Done = true
		out.Events = []Adjustment{}
		incident := out.Event.IncidentID
		out.IncidentID = incident
		if incident == "" {
			if cursor != "" {
				return ErrInvalid
			}
			out.Events = append(out.Events, out.Event)
			return nil
		}
		for _, active := range c.data.Incidents {
			out.IncidentActive = out.IncidentActive || active == incident
		}
		prefix := incident + "/"
		if cursor != "" {
			if !strings.HasPrefix(cursor, prefix) {
				return ErrInvalid
			}
			if _, err := strconv.ParseUint(strings.TrimPrefix(cursor, prefix), 10, 64); err != nil {
				return ErrInvalid
			}
		}
		if err := tx.Scan("node-rate-incidents", prefix, "", 1, func(_ string, raw json.RawMessage) error {
			var trigger Adjustment
			if err := json.Unmarshal(raw, &trigger); err != nil {
				return err
			}
			out.TriggerEvent = &trigger
			return nil
		}); err != nil {
			return err
		}
		out.HistoryComplete = out.TriggerEvent != nil
		last := ""
		return tx.Scan("node-rate-incidents", prefix, cursor, 101, func(key string, raw json.RawMessage) error {
			if len(out.Events) == 100 {
				out.Done = false
				out.NextCursor = last
				return nil
			}
			var event Adjustment
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			out.Events = append(out.Events, event)
			last = key
			return nil
		})
	})
	return out, err
}
