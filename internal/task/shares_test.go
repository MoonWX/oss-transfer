package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// shareParent builds a running parent whose demand is driven only by the
// per-node child counts: its scan is finished and fully dispatched.
func shareParent(n int, spec func(*Spec)) Parent {
	s := testSpec()
	s.Nodes = nil
	for i := range n {
		id := fmt.Sprintf("n%02d", i)
		s.Nodes = append(s.Nodes, Node{ID: id, EnrollmentID: "e" + id, Address: id})
	}
	if spec != nil {
		spec(&s)
	}
	p := Parent{ID: 1, Spec: s, Generation: 1, Status: "RUNNING", ScanComplete: true, Scheduling: &SchedulingState{Nodes: map[string]PendingLoad{}}}
	seedShares(&p)
	if err := bumpBudget(&p); err != nil {
		panic(err)
	}
	return p
}

func setBusy(p *Parent, busy map[string]bool) {
	p.Scheduling = &SchedulingState{Nodes: map[string]PendingLoad{}}
	for id, b := range busy {
		if b {
			p.Scheduling.Nodes[id] = PendingLoad{Children: 1, Objects: 1}
		}
	}
}

func shareColumn(p Parent, d int) []float64 {
	var out []float64
	for _, node := range p.ActiveNodes() {
		out = append(out, p.Shares.Nodes[node.ID].Share.value(d))
	}
	return out
}

// settle acknowledges every node and runs passes until nothing changes.
func settle(t *testing.T, p *Parent, idle map[string]bool) {
	t.Helper()
	for range 8 {
		acked := map[string]bool{}
		for _, node := range p.ActiveNodes() {
			acked[node.ID] = true
		}
		if !reallocate(p, acked, idle) {
			return
		}
		if err := bumpBudget(p); err != nil {
			t.Fatal(err)
		}
		if err := validateShares(*p); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("allocation did not settle")
}

// Targets for random totals, node counts and demand: shares stay positive,
// within the total and the per-node caps; idle nodes get exactly the floor;
// busy nodes split the rest max-min fairly.
func TestShareTargetsProperties(t *testing.T) {
	random := rand.New(rand.NewPCG(7, 11))
	for iteration := range 4000 {
		n := 1 + random.IntN(16)
		bandwidth := float64(n) + math.Floor(random.Float64()*float64(int64(1)<<40))
		if iteration%5 == 0 {
			bandwidth = float64(n) // exact lower bound
		}
		qps := math.Pow(10, -3+9*random.Float64())
		if iteration%7 == 0 {
			qps = 0.001
		}
		perNode := iteration%3 == 0
		legacy := 0.0
		if iteration%4 == 0 {
			legacy = 8 * (float64(n) + math.Floor(random.Float64()*float64(int64(1)<<38)))
		}
		limits := &execution.RequestLimits{Scope: "CLUSTER", ListQPS: qps, HeadQPS: qps * 2, PutQPS: qps / 3, BandwidthBitsPerSecond: legacy}
		if perNode {
			limits.Scope = "PER_NODE"
		}
		p := shareParent(n, func(s *Spec) { s.BandwidthMbps = bandwidth / 125000; s.RequestLimits = limits })
		busy := map[string]bool{}
		for _, node := range p.ActiveNodes() {
			busy[node.ID] = random.IntN(3) > 0
		}
		setBusy(&p, busy)
		settle(t, &p, IdleNow(p))
		l := limitsOf(p.Spec)
		if perNode && l.cap[1] != qps || !perNode && l.total[1] != qps {
			t.Fatal("QPS scope", l)
		}
		if legacy > 0 && !perNode && l.total[0] != math.Min(math.Floor(bandwidth/125000*125000), math.Floor(legacy/8)) {
			t.Fatal("legacy and Mbps totals must take the smaller", l, bandwidth, legacy)
		}
		anyBusy := false
		for _, b := range busy {
			anyBusy = anyBusy || b
		}
		for d := range shareDimensions {
			column := shareColumn(p, d)
			if l.total[d] == 0 {
				for _, v := range column {
					if v != l.cap[d] {
						t.Fatalf("undivided dimension %d changed: %v cap %v", d, column, l.cap[d])
					}
				}
				continue
			}
			if sum(column) > l.total[d] {
				t.Fatalf("iteration %d dimension %d: sum %v exceeds %v", iteration, d, sum(column), l.total[d])
			}
			floor := idleFloor(l.total[d], l.cap[d], n, integerDimension(d))
			busyLevel := -1.0
			for i, node := range p.ActiveNodes() {
				v := column[i]
				if v <= 0 || l.cap[d] > 0 && v > l.cap[d] {
					t.Fatalf("dimension %d node %s share %v", d, node.ID, v)
				}
				if anyBusy && !busy[node.ID] && v != floor {
					t.Fatalf("idle node %s got %v, floor %v", node.ID, v, floor)
				}
				if anyBusy && busy[node.ID] && (l.cap[d] == 0 || v < l.cap[d]) {
					// Uncapped busy nodes differ by at most one unit (or float rounding).
					if busyLevel < 0 {
						busyLevel = v
					} else if math.Abs(v-busyLevel) > 1 && math.Abs(v-busyLevel) > 1e-9*busyLevel {
						t.Fatalf("busy nodes not max-min fair: %v", column)
					}
				}
			}
		}
	}
}

func TestShareTargetsExactBoundaries(t *testing.T) {
	// ListQPS 0.001 over seven nodes: every float share is positive and the
	// float sum does not exceed the total.
	p := shareParent(7, func(s *Spec) { s.RequestLimits = &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 0.001} })
	for _, busy := range []int{0, 1, 3, 7} {
		set := map[string]bool{}
		for i, node := range p.ActiveNodes() {
			set[node.ID] = i < busy
		}
		setBusy(&p, set)
		settle(t, &p, IdleNow(p))
		column := shareColumn(p, 1)
		if sum(column) > 0.001 {
			t.Fatal("float shares exceed total", column)
		}
		for _, v := range column {
			if v < minimumQPS {
				t.Fatal("share below the Agent minimum", column)
			}
		}
	}
	// A bandwidth total equal to the node count: one byte per second each,
	// busy or idle.
	q := shareParent(5, func(s *Spec) { s.BandwidthMbps = 5.0 / 125000 })
	setBusy(&q, map[string]bool{"n00": true})
	settle(t, &q, IdleNow(q))
	for _, v := range shareColumn(q, 0) {
		if v != 1 {
			t.Fatal("T=N must give each node one byte per second", shareColumn(q, 0))
		}
	}
	// The user-reported case: 1000 Mbps, one node draining its last child.
	r := shareParent(2, func(s *Spec) { s.BandwidthMbps = 1000 })
	setBusy(&r, map[string]bool{"n00": true})
	settle(t, &r, IdleNow(r))
	total := 1000.0 * 125000
	floor := math.Floor(total / 128)
	if got := shareColumn(r, 0); got[1] != floor || got[0] != total-floor {
		t.Fatal("busy node must receive everything but the idle floor", got)
	}
}

func TestShareBusyRules(t *testing.T) {
	p := shareParent(3, func(s *Spec) { s.BandwidthMbps = 10 })
	all := func(p Parent) bool {
		for _, b := range ShareBusy(p) {
			if !b {
				return false
			}
		}
		return true
	}
	setBusy(&p, map[string]bool{"n00": true})
	if !ShareBusy(p)["n00"] || ShareBusy(p)["n01"] {
		t.Fatal("finished scan: only nodes with children are busy", ShareBusy(p))
	}
	scanning := p
	scanning.ScanComplete = false
	if b := ShareBusy(scanning); !b["n01"] || !b["n02"] {
		t.Fatal("placement nodes are busy while the scan runs", b)
	}
	undispatched := p
	undispatched.ScannedBatches = 3
	if !all(undispatched) {
		t.Fatal("placement nodes are busy while batches wait for dispatch")
	}
	affinity := scanning
	affinity.Spec.DistributionMode = "AFFINITY"
	if b := ShareBusy(affinity); !b["n00"] || b["n01"] || b["n02"] {
		t.Fatal("affinity places new work on its first node only", b)
	}
	for name, mutate := range map[string]func(*Parent){
		"scheduling missing": func(p *Parent) { p.Scheduling = nil },
		"failover pending": func(p *Parent) {
			p.Failover = &FailoverState{Enabled: true, Migrations: map[string]FailoverMigration{"x": {}}}
		},
		"scanner seeding": func(p *Parent) { p.Failover = &FailoverState{Enabled: true, ScannerSeeding: true} },
		"not running":     func(p *Parent) { p.Status = "RESUMING" },
		"resume prepare":  func(p *Parent) { p.ResumeAfter = new(int) },
	} {
		q := p
		mutate(&q)
		if !all(q) {
			t.Fatal(name, "must count every node as busy")
		}
	}
	handoff := p
	handoff.Failover = &FailoverState{Enabled: true, Rebalance: &RebalanceState{Pending: &QueuedHandoff{Source: Node{ID: "n00"}, Target: Node{ID: "n02"}}}}
	if b := ShareBusy(handoff); !b["n02"] {
		t.Fatal("a queued handoff target is busy", b)
	}
	draining := p
	draining.Failover = &FailoverState{Enabled: true}
	draining.Membership = &MembershipState{Draining: map[string]NodeDrain{"n01": {Mode: DrainModeDrain}}}
	if b := ShareBusy(draining); b["n01"] {
		// The drain arms queue rebalancing, which keeps placement nodes busy;
		// the draining node itself is not a placement node.
		t.Fatal("a draining node without children is idle", b)
	}
	setBusy(&draining, map[string]bool{"n01": true})
	if b := ShareBusy(draining); !b["n01"] {
		t.Fatal("a draining node with children is busy", b)
	}
}

func TestSharesValidationRejectsMissingNode(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	spec := testSpec()
	spec.BandwidthMbps = 8
	p, _, err := repo.Create(spec, "shares")
	if err != nil {
		t.Fatal(err)
	}
	if p.Shares == nil || p.BudgetRevision != 2 || p.Shares.Revision != 1 {
		t.Fatal("a new task starts with shares one revision below its budget", p.Shares, p.BudgetRevision)
	}
	for name, mutate := range map[string]func(*Parent){
		"missing node":     func(p *Parent) { delete(p.Shares.Nodes, "b") },
		"revision":         func(p *Parent) { p.BudgetRevision++ },
		"above reserve":    func(p *Parent) { e := p.Shares.Nodes["a"]; e.Share.Bandwidth = e.Reserved.Bandwidth + 1; p.Shares.Nodes["a"] = e },
		"sum above total":  func(p *Parent) { e := p.Shares.Nodes["a"]; e.Share.Bandwidth += 1 << 20; e.Reserved = e.Share; p.Shares.Nodes["a"] = e },
		"zero share":       func(p *Parent) { e := p.Shares.Nodes["a"]; e.Share.Bandwidth = 0; p.Shares.Nodes["a"] = e },
		"retired, kept":    func(p *Parent) { p.Failover = &FailoverState{Enabled: true, Retired: map[string]RetiredNode{"b": {}}} },
		"undivided capped": func(p *Parent) { e := p.Shares.Nodes["a"]; e.Share.List = 3; e.Reserved.List = 3; p.Shares.Nodes["a"] = e },
	} {
		current, _ := repo.Get(p.ID)
		data, _ := json.Marshal(current)
		var copy Parent
		_ = json.Unmarshal(data, &copy)
		mutate(&copy)
		err := db.Update(func(tx *state.Tx) error { return repo.save(tx, copy) })
		if err == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// Every commit keeps BudgetRevision one above the delivered revision, so an
// older Master that sends its equal split at BudgetRevision is accepted by a
// real Agent limiter instead of conflicting with a revision already used.
func TestStampKeepsBudgetAheadOfWire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer db.Close()
	spec := testSpec()
	spec.BandwidthMbps = 8
	spec.RequestLimits = &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 10}
	p, _, err := repo.Create(spec, "stamp")
	if err != nil {
		t.Fatal(err)
	}
	agentDB, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer agentDB.Close()
	limiter, err := ratelimit.New(agentDB, ratelimit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Close()
	ctx := context.Background()
	check := func(p Parent) {
		t.Helper()
		if p.BudgetRevision != WireRevision(p)+1 {
			t.Fatal("budget does not lead the wire revision", p.BudgetRevision, WireRevision(p))
		}
		if err := limiter.ApplyParent(ctx, p.NodePolicy(0)); err != nil {
			t.Fatal("current share rejected", err)
		}
		old := p
		old.Shares = nil // an older Master drops the field
		if err := limiter.ApplyParent(ctx, old.NodePolicy(0)); err != nil {
			t.Fatal("older Master's equal split rejected", err)
		}
	}
	check(p)
	ten := 10.0
	if p, err = repo.UpdateLimits(p.ID, &ten, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	check(p)
	all := []string{"a", "b"}
	if _, err := repo.Assign(p.ID, p.Generation, 1, objects("x")); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Get(p.ID)
	if err := db.Update(func(tx *state.Tx) error {
		current, err := get(tx, p.ID)
		if err != nil {
			return err
		}
		current.ScanComplete, current.ScannedBatches = true, current.DispatchedThrough
		return save(tx, current)
	}); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Get(p.ID)
	next, changed, err := repo.ReallocateShares(p.ID, p.Generation, WireRevision(p), all, map[string]bool{"a": true, "b": true})
	if err != nil || !changed {
		t.Fatal("idle node was not reduced", changed, err)
	}
	// Each of the next limiter checks sends both values to a fresh limiter
	// view; recreate it so a newer revision from check() does not hide this one.
	check(next)
	if p, _, err = repo.RequestStop(p.ID); err != nil {
		t.Fatal(err)
	}
	check(p)
}

func TestLegacyMaterializeRequiresAllAcks(t *testing.T) {
	db, repo := openTest(t, filepath.Join(t.TempDir(), "master.db"))
	defer db.Close()
	spec := testSpec()
	spec.BandwidthMbps = 8
	spec.RequestLimits = &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 0.3}
	p, _, err := repo.Create(spec, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	// Model a task stored by an older Master: no shares, equal split at
	// BudgetRevision.
	if err := db.Update(func(tx *state.Tx) error {
		current, err := get(tx, p.ID)
		if err != nil {
			return err
		}
		current.Shares, current.BudgetRevision = nil, 5
		current.ScanComplete = true
		current.Scheduling = &SchedulingState{Nodes: map[string]PendingLoad{"a": {Children: 1, Objects: 1}}}
		return save(tx, current)
	}); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Get(p.ID)
	if WireRevision(p) != 5 || p.NodePolicy(1).BandwidthBytesPerSecond != 500000 {
		t.Fatal("an equal-split task keeps its policy", p.NodePolicy(1))
	}
	idle := map[string]bool{"b": true}
	if _, changed, err := repo.ReallocateShares(p.ID, p.Generation, 5, []string{"a"}, idle); err != nil || changed {
		t.Fatal("converted before every node applied the equal split", changed, err)
	}
	p, changed, err := repo.ReallocateShares(p.ID, p.Generation, 5, []string{"a", "b"}, idle)
	if err != nil || !changed || p.Shares == nil || WireRevision(p) <= 5 || p.BudgetRevision != WireRevision(p)+1 {
		t.Fatal("not converted", changed, err, p.Shares)
	}
	b := p.Shares.Nodes["b"]
	if b.Share.Bandwidth >= 500000 || b.Reserved.Bandwidth != 500000 {
		t.Fatal("the idle node must reduce first and keep its old share reserved", b)
	}
	a := p.Shares.Nodes["a"]
	if a.Share.Bandwidth != 500000 {
		t.Fatal("no raise before the reduction is acknowledged", a)
	}
	if b.Reserved.List != 0.15 || sum([]float64{a.Share.List, b.Share.List}) > 0.3 {
		t.Fatal("float equal split reserved exactly, delivered within the total", a, b)
	}
}

// --- Model check ---------------------------------------------------------

// modelAgent applies policies with the rules of ratelimit.Limiter.ApplyParent.
type modelAgent struct {
	installed  *ratelimit.ParentLimit
	authorized bool // holds a task lease: may transfer
}

var errModelStopped = errors.New("stopped")

func (a *modelAgent) apply(p ratelimit.ParentLimit) error {
	if old := a.installed; old != nil {
		if p.Generation < old.Generation || p.Generation == old.Generation && p.Revision < old.Revision {
			return ratelimit.ErrStale
		}
		if p.Generation == old.Generation && p.Revision == old.Revision {
			if p != *old {
				return ratelimit.ErrConflict
			}
			return nil
		}
		if p.Generation == old.Generation && old.Stopped && !p.Stopped {
			return errModelStopped
		}
	}
	a.installed = &p
	return nil
}

// accepts reports whether p would still be installed if delivered now.
func (a *modelAgent) accepts(p ratelimit.ParentLimit) bool {
	copy := *a
	return copy.apply(p) == nil && (a.installed == nil || *a.installed != p)
}

func policyValues(p ratelimit.ParentLimit) [shareDimensions]float64 {
	if p.Stopped {
		return [shareDimensions]float64{}
	}
	return [shareDimensions]float64{float64(p.BandwidthBytesPerSecond), p.ListQPS, p.HeadQPS, p.PutQPS}
}

type modelCommand struct {
	node   string
	policy ratelimit.ParentLimit
}

type modelReply struct {
	node   string
	policy ratelimit.ParentLimit
	epoch  int // Master process that sent it
}

type shareModel struct {
	t        *testing.T
	random   *rand.Rand
	p        Parent
	history  []Parent // persisted versions; any may still be sent
	agents   map[string]*modelAgent
	commands []modelCommand
	replies  []modelReply
	acks     map[string]ratelimit.ParentLimit
	epoch    int
	bound    [shareDimensions]float64 // allowed sum while totals change
	foreign  bool                     // an older Master delivered its own split
	skipped  uint64                   // revision every node must pass to clear foreign
	log      []string
}

func (m *shareModel) commit(p Parent) {
	m.t.Helper()
	if err := bumpBudget(&p); err != nil {
		m.t.Fatal(err)
	}
	if err := validateShares(p); err != nil {
		m.t.Fatal(err)
	}
	m.p = p
	m.history = append(m.history, p)
	if len(m.history) > 12 {
		m.history = m.history[1:]
	}
}

func (m *shareModel) acked() map[string]bool {
	acked := map[string]bool{}
	for _, node := range m.p.ActiveNodes() {
		if ack, ok := m.acks[node.ID]; ok && ack.Generation == m.p.Generation && ack.Revision == WireRevision(m.p) {
			acked[node.ID] = true
		}
	}
	return acked
}

// clone deep-copies through JSON, as the database does.
func clone(p Parent) Parent {
	data, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var out Parent
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

func (m *shareModel) send(p Parent, node string) {
	if policy, ok := p.NodePolicyFor(node); ok {
		m.commands = append(m.commands, modelCommand{node: node, policy: policy})
	}
}

func (m *shareModel) deliver(index int) {
	m.t.Helper()
	command := m.commands[index]
	m.commands = append(m.commands[:index], m.commands[index+1:]...)
	err := m.agents[command.node].apply(command.policy)
	if errors.Is(err, ratelimit.ErrConflict) {
		m.t.Fatalf("revision conflict at node %s: %+v vs %+v", command.node, command.policy, *m.agents[command.node].installed)
	}
	if err == nil && m.random.IntN(4) > 0 { // a quarter of replies are lost
		m.replies = append(m.replies, modelReply{node: command.node, policy: command.policy, epoch: m.epoch})
	}
}

func (m *shareModel) totals() [shareDimensions]float64 { return limitsOf(m.p.Spec).total }

// held is the most a node may enforce now or later without a new commit: its
// installed value or any value in flight to it that it would still accept.
func (m *shareModel) held(id string) [shareDimensions]float64 {
	agent := m.agents[id]
	var held [shareDimensions]float64
	if agent.installed != nil && agent.installed.Generation == m.p.Generation {
		held = policyValues(*agent.installed)
	}
	for _, command := range m.commands {
		if command.node == id && agent.accepts(command.policy) {
			values := policyValues(command.policy)
			for d := range held {
				held[d] = math.Max(held[d], values[d])
			}
		}
	}
	return held
}

// check asserts the invariant for the nodes that may transfer (they hold a
// lease): each one holds at most its reserve (installed, or in flight and
// still acceptable), and their reserves sum within the total. After a total
// changes, the reserves sum within the larger of the old and new totals, and
// that bound only decreases until it is the new total. The enforced sum is
// therefore within the bound at every moment.
func (m *shareModel) check() {
	m.t.Helper()
	totals := m.totals()
	var enforced, reserves [shareDimensions]float64
	for _, node := range m.p.ActiveNodes() {
		agent := m.agents[node.ID]
		if !agent.authorized {
			continue
		}
		held := m.held(node.ID)
		entry := m.p.Shares.Nodes[node.ID]
		for d := range held {
			if totals[d] == 0 {
				continue
			}
			if !m.foreign && held[d] > entry.Reserved.value(d) {
				m.t.Fatalf("node %s dimension %d holds %v above its reserve %v\n%v", node.ID, d, held[d], entry.Reserved.value(d), m.log[max(0, len(m.log)-12):])
			}
			enforced[d] += held[d]
			reserves[d] += entry.Reserved.value(d)
		}
	}
	if m.foreign {
		// An older Master's equal split is outside the ledger by design; the
		// check resumes once every member applied a revision past the skip,
		// whose reserves cover that split.
		for _, node := range m.p.ActiveNodes() {
			installed := m.agents[node.ID].installed
			if installed == nil || installed.Generation != m.p.Generation || installed.Revision <= m.skipped {
				return
			}
		}
		m.foreign = false
		for d := range m.bound {
			m.bound[d] = math.Max(m.bound[d], reserves[d])
		}
	}
	for d := range totals {
		if totals[d] == 0 {
			continue
		}
		bound := math.Max(m.bound[d], totals[d]) * (1 + 1e-12) // float sums may differ by ulps
		if reserves[d] > bound || enforced[d] > bound {
			var detail []string
			for _, node := range m.p.ActiveNodes() {
				detail = append(detail, fmt.Sprintf("%s auth=%v installed=%+v held=%v", node.ID, m.agents[node.ID].authorized, m.agents[node.ID].installed, m.held(node.ID)))
			}
			m.t.Fatalf("dimension %d: reserves %v, enforced %v exceed %v (total %v)\nshares %+v\n%v\n%v", d, reserves[d], enforced[d], bound, totals[d], m.p.Shares, detail, m.log[max(0, len(m.log)-12):])
		}
		m.bound[d] = math.Max(totals[d], math.Min(m.bound[d], reserves[d]))
	}
}

func TestShareLedgerModelCheck(t *testing.T) {
	seeds := 200
	if testing.Short() {
		seeds = 10
	}
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { runShareModel(t, uint64(seed)) })
	}
}

func runShareModel(t *testing.T, seed uint64) {
	random := rand.New(rand.NewPCG(seed, 99))
	m := &shareModel{t: t, random: random, agents: map[string]*modelAgent{}, acks: map[string]ratelimit.ParentLimit{}}
	p := shareParent(5, func(s *Spec) {
		s.BandwidthMbps = float64(1+random.IntN(1000)) / 8
		s.RequestLimits = &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 0.5 + float64(random.IntN(100))}
	})
	p.Failover = &FailoverState{Enabled: true, Retired: map[string]RetiredNode{}}
	m.p, m.history = p, []Parent{p}
	for _, node := range p.Spec.Nodes {
		policy, _ := p.NodePolicyFor(node.ID)
		// Creation: every original member applies its first share before work.
		m.agents[node.ID] = &modelAgent{installed: &policy, authorized: true}
		m.acks[node.ID] = policy
	}
	m.bound = m.totals()
	members := func() []string {
		var ids []string
		for _, node := range m.p.ActiveNodes() {
			ids = append(ids, node.ID)
		}
		return ids
	}
	for step := range 600 {
		action := random.IntN(100)
		m.log = append(m.log, fmt.Sprintf("%d:%d rev=%d status=%s", step, action, WireRevision(m.p), m.p.Status))
		switch {
		case action < 22: // a send path delivers a (possibly stale) snapshot
			version := m.history[random.IntN(len(m.history))]
			if random.IntN(2) == 0 {
				version = m.p
			}
			ids := members()
			m.send(version, ids[random.IntN(len(ids))])
		case action < 42:
			if len(m.commands) > 0 {
				m.deliver(random.IntN(len(m.commands)))
			}
		case action < 54:
			if len(m.replies) > 0 {
				i := random.IntN(len(m.replies))
				reply := m.replies[i]
				m.replies = append(m.replies[:i], m.replies[i+1:]...)
				last, ok := m.acks[reply.node]
				if reply.epoch == m.epoch && (!ok || reply.policy.Generation > last.Generation || reply.policy.Generation == last.Generation && reply.policy.Revision > last.Revision) {
					m.acks[reply.node] = reply.policy
				}
			}
		case action < 66: // allocation pass with random demand and debounce
			if m.p.Status != "RUNNING" {
				continue
			}
			idle := map[string]bool{}
			for _, id := range members() {
				idle[id] = random.IntN(2) == 0
			}
			next := clone(m.p)
			if reallocate(&next, m.acked(), idle) {
				m.commit(next)
				for _, id := range members() {
					m.send(m.p, id)
				}
			}
		case action < 74: // demand changes
			next := clone(m.p)
			ids := members()
			id := ids[random.IntN(len(ids))]
			load := next.Scheduling.Nodes[id]
			load.Children = int64(random.IntN(2))
			next.Scheduling.Nodes[id] = load
			m.p = next // scheduling changes are commits without a policy change
		case action < 77: // Master restart: durable state survives, acks do not
			m.p = clone(m.p)
			m.acks = map[string]ratelimit.ParentLimit{}
			m.epoch++
		case action < 81: // the task limits change
			if m.p.Status != "RUNNING" && m.p.Status != "STOPPING" {
				continue
			}
			next := clone(m.p)
			old := next.Spec
			next.Spec.BandwidthMbps = float64(1+random.IntN(1000)) / 8
			limits := *next.Spec.RequestLimits
			limits.ListQPS = 0.5 + float64(random.IntN(100))
			next.Spec.RequestLimits = &limits
			if next.Shares != nil {
				changeShareLimits(&next, old)
			}
			before := m.totals()
			m.commit(next)
			after := m.totals()
			for d := range m.bound {
				m.bound[d] = math.Max(math.Max(m.bound[d], before[d]), after[d])
			}
		case action < 85: // a member is fenced (lease expiry or removal) and leaves
			ids := members()
			if len(ids) < 2 || m.p.Status != "RUNNING" || m.p.Shares == nil {
				continue
			}
			id := ids[random.IntN(len(ids))]
			m.agents[id].authorized = false
			next := clone(m.p)
			if next.Failover.Retired == nil {
				next.Failover.Retired = map[string]RetiredNode{}
			}
			next.Failover.Retired[id] = RetiredNode{}
			leaveShares(&next, id)
			m.commit(next)
		case action < 89: // a retired member rejoins; it is granted only through the gate
			if m.p.Status != "RUNNING" || m.p.Shares == nil {
				continue
			}
			var retired []string
			for id := range m.p.Failover.Retired {
				retired = append(retired, id)
			}
			if len(retired) == 0 {
				continue
			}
			id := retired[random.IntN(len(retired))]
			next := clone(m.p)
			delete(next.Failover.Retired, id)
			rejoinShares(&next, id)
			m.commit(next)
		case action < 93: // lease gate for rejoined members
			for _, id := range members() {
				if m.agents[id].authorized || m.p.Shares == nil {
					continue
				}
				next := clone(m.p)
				if foldShares(&next, m.acked()) {
					if err := validateShares(next); err != nil {
						t.Fatal(err)
					}
					m.p = next // a fold changes no delivered value or revision
				}
				if m.acked()[id] && SharesFit(m.p) {
					m.agents[id].authorized = true
				}
			}
		case action < 95: // stop, and resume once every member applied the stop
			switch m.p.Status {
			case "RUNNING":
				next := clone(m.p)
				next.Status = "STOPPING"
				m.commit(next)
			case "STOPPING":
				ready := true
				for _, id := range members() {
					installed := m.agents[id].installed
					ready = ready && (!m.agents[id].authorized || installed != nil && installed.Generation == m.p.Generation && installed.Stopped)
				}
				if ready {
					next := clone(m.p)
					next.Status = "RUNNING"
					next.Generation++
					seedShares(&next)
					m.commit(next)
					m.bound = m.totals()
				}
			}
		case action < 97: // an older Master runs: it drops the shares and sends its equal split
			if m.p.Shares == nil || m.p.Status != "RUNNING" {
				continue
			}
			old := clone(m.p)
			old.Shares = nil
			ids := members()
			id := ids[random.IntN(len(ids))]
			if policy, ok := old.NodePolicyFor(id); ok {
				if err := m.agents[id].apply(policy); errors.Is(err, ratelimit.ErrConflict) {
					t.Fatal("older Master's revision conflicts", err)
				}
			}
			m.foreign = true
			// Back on this release: the Agent now rejects the share revision as
			// stale, which the runtime answers by skipping past it.
			held := m.agents[id].installed.Revision
			if m.agents[id].installed.Generation == m.p.Generation && held >= WireRevision(m.p) {
				next := clone(m.p)
				legacy := clone(m.p)
				legacy.Shares, legacy.BudgetRevision = nil, held
				for i, node := range next.ActiveNodes() {
					policy := legacy.NodePolicy(i)
					entry := next.Shares.Nodes[node.ID]
					values := policyValues(policy)
					for d := range shareDimensions {
						if limitsOf(next.Spec).total[d] > 0 {
							entry.Reserved.setValue(d, math.Max(entry.Reserved.value(d), values[d]))
						}
					}
					next.Shares.Nodes[node.ID] = entry
				}
				next.BudgetRevision = held + 1
				m.commit(next)
				m.skipped = held
			}
		default: // resend the current policy to every member
			for _, id := range members() {
				m.send(m.p, id)
			}
		}
		_ = step
		m.check()
	}
	// Liveness: with every member online and acknowledging, demand settles.
	for range 20 {
		for _, id := range members() {
			m.send(m.p, id)
		}
		for len(m.commands) > 0 {
			m.deliver(0)
		}
		for _, reply := range m.replies {
			if reply.epoch == m.epoch {
				m.acks[reply.node] = reply.policy
			}
		}
		m.replies = nil
		for _, id := range members() {
			if policy, ok := m.p.NodePolicyFor(id); ok && m.agents[id].installed != nil && *m.agents[id].installed == policy {
				m.acks[id] = policy
			}
		}
		if m.p.Status == "RUNNING" {
			next := clone(m.p)
			if reallocate(&next, m.acked(), IdleNow(next)) {
				m.commit(next)
			}
		}
		m.check()
	}
}
