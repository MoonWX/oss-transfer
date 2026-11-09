package master

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

// comparisonAssignment maps a comparison to the Agent enrollment that runs it
// and keeps its results. The Master stores no comparison results itself.
type comparisonAssignment struct {
	Request      comparison.Request `json:"request"`
	NodeID       string             `json:"nodeId"`
	EnrollmentID string             `json:"enrollmentId"`
	// CreatedAt is zero for assignments recorded by an older Master.
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

const comparisonAssignmentsBucket = "comparison_assignments"

// Timing of the comparison API: variables only so that tests can shorten them.
var (
	// comparisonPageBudget bounds how long one page request keeps asking an
	// Agent for more rows to fill a filtered page.
	comparisonPageBudget = 8 * time.Second
	// comparisonCallTimeout bounds one Agent call of the interactive routes.
	comparisonCallTimeout = 15 * time.Second
	// comparisonListTimeout bounds the work for one node in a list request.
	comparisonListTimeout = 10 * time.Second
	// comparisonForgetGrace keeps an assignment that its Agent does not list
	// or know yet: the Master records it before the Agent starts the
	// comparison, so a list request can run in between.
	comparisonForgetGrace = 2 * time.Minute
)

var (
	errComparisonNodeRemoved = errors.New("The comparison node has been removed")
	errComparisonPage        = errors.New("Agent returned an invalid comparison page")
)

// comparisonAPI serves /api/master/comparisons. Agents keep the results; the
// Master routes requests to the Agent recorded for each comparison and
// speaks the older operations to Agents before comparison.Protocol.
type comparisonAPI struct {
	server    *Server
	now       func() time.Time
	protocols comparisonProtocols
	tickets   comparisonTickets
}

func newComparisonAPI(s *Server) *comparisonAPI {
	return &comparisonAPI{server: s, now: s.options.Now, tickets: comparisonTickets{issued: map[string]comparisonTicket{}}}
}

func (c *comparisonAPI) start(w http.ResponseWriter, r *http.Request) {
	s := c.server
	var input struct {
		SourceBucket      string `json:"sourceBucket"`
		DestinationBucket string `json:"destBucket"`
		SourcePrefix      string `json:"sourcePrefix"`
		DestinationPrefix string `json:"destPrefix"`
		Mode              string `json:"mode"`
		Scope             string `json:"scope"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	a, err := comparisonEndpoint(input.SourceBucket, snapshot)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	b, err := comparisonEndpoint(input.DestinationBucket, snapshot)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	id, err := comparisonID()
	if err != nil {
		fail(w, 500, "Cannot create comparison")
		return
	}
	request := comparison.Request{ID: id, StorageRevision: snapshot.Revision, Source: a, Destination: b, SourcePrefix: input.SourcePrefix, DestinationPrefix: input.DestinationPrefix, Mode: input.Mode, Scope: input.Scope}
	if err = comparison.Validate(request); err != nil {
		fail(w, 400, err.Error())
		return
	}
	nodes, err := s.onlineAgents()
	if err != nil {
		fail(w, 500, "Cannot read Agent state")
		return
	}
	if len(nodes) == 0 {
		fail(w, 503, "No connected Agent is available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	for _, node := range c.preferComparisonProtocol(ctx, nodes) {
		if err = s.syncStorage(ctx, node, snapshot); err != nil {
			continue
		}
		assignment := comparisonAssignment{Request: request, NodeID: node.ID, EnrollmentID: node.EnrollmentID, CreatedAt: c.now().UTC()}
		if err = s.store.Update(func(tx *state.Tx) error { return tx.Put(comparisonAssignmentsBucket, id, assignment) }); err != nil {
			fail(w, 500, "Cannot save comparison")
			return
		}
		var progress comparison.Progress
		err = s.agentCall(ctx, node.Session(), "comparison_start", true, request, &progress)
		if agentStatus(err, 429) || comparisonUnsupported(err) || agentUpdating(err) {
			// These replies explicitly reject admission. Do not leave a mapping
			// to work which was never started, even if every Agent is busy.
			if err := c.forget(id); err != nil {
				fail(w, 500, "Cannot save comparison")
				return
			}
			continue
		}
		if err != nil {
			// The durable assignment remains discoverable after an uncertain
			// response; never start the same comparison on another node.
			writeJSON(w, 502, map[string]any{"success": false, "id": id, "message": redactStorageError(err, snapshot.Connections).Error()})
			return
		}
		comparisonProgressReply(w, &progress)
		writeJSON(w, 202, progress)
		return
	}
	fail(w, 503, "No Agent is available for comparison; a comparison may already be running")
}

// comparisonID draws a new comparison ID. It never starts with '-', so that
// command lines do not take it for an option.
func comparisonID() (string, error) {
	for {
		id, err := randomToken()
		if err != nil || !strings.HasPrefix(id, "-") {
			return id, err
		}
	}
}

// preferComparisonProtocol orders nodes so that Agents with
// comparison.Protocol come first and Agents that did not answer come last,
// keeping the registry order within each group.
func (c *comparisonAPI) preferComparisonProtocol(ctx context.Context, nodes []cluster.Member) []cluster.Member {
	tiers := make([]int, len(nodes))
	slots := make(chan struct{}, 8)
	var group sync.WaitGroup
	for i, node := range nodes {
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				tiers[i] = 2
				return
			}
			protocol, err := c.comparisonProtocol(ctx, node.Session())
			switch {
			case err != nil:
				tiers[i] = 2
			case protocol < comparison.Protocol:
				tiers[i] = 1
			}
		}()
	}
	group.Wait()
	ordered := make([]cluster.Member, 0, len(nodes))
	for tier := range 3 {
		for i, node := range nodes {
			if tiers[i] == tier {
				ordered = append(ordered, node)
			}
		}
	}
	return ordered
}

func (c *comparisonAPI) forget(ids ...string) error {
	return c.server.store.Update(func(tx *state.Tx) error {
		for _, id := range ids {
			if err := tx.Delete(comparisonAssignmentsBucket, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// comparisonNode returns the assignment of comparison id and its Agent. An
// assignment of a node that was enrolled again under the same node ID, and so
// kept its data directory and results, moves to the current enrollment.
func (c *comparisonAPI) comparisonNode(id string) (comparisonAssignment, cluster.Member, error) {
	var a comparisonAssignment
	err := c.server.store.View(func(tx *state.Tx) error {
		found, err := tx.Get(comparisonAssignmentsBucket, id, &a)
		if err != nil {
			return err
		}
		if !found {
			return comparison.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return a, cluster.Member{}, err
	}
	members, err := c.server.agents.registry.Members()
	if err != nil {
		return a, cluster.Member{}, err
	}
	for _, member := range members {
		if member.ID != a.NodeID {
			continue
		}
		if member.EnrollmentID != a.EnrollmentID {
			if a, err = c.rebind(a, member); err != nil {
				return a, cluster.Member{}, err
			}
		}
		return a, member, nil
	}
	return a, cluster.Member{}, errComparisonNodeRemoved
}

// rebind records member's current enrollment in assignment a of the same
// node, unless the assignment changed or disappeared meanwhile.
func (c *comparisonAPI) rebind(a comparisonAssignment, member cluster.Member) (comparisonAssignment, error) {
	err := c.server.store.Update(func(tx *state.Tx) error {
		var current comparisonAssignment
		found, err := tx.Get(comparisonAssignmentsBucket, a.Request.ID, &current)
		if err != nil {
			return err
		}
		if !found {
			return comparison.ErrNotFound
		}
		if current.NodeID != member.ID {
			return errComparisonNodeRemoved
		}
		current.EnrollmentID = member.EnrollmentID
		a = current
		return tx.Put(comparisonAssignmentsBucket, a.Request.ID, current)
	})
	return a, err
}

// comparisonMember resolves the Agent of comparison id for a route that
// talks to it, or replies itself. A comparison whose node was removed went
// with it: its assignment is forgotten and it is reported as not found, or as
// deleted when deleting is true. An offline Agent is reported at once instead
// of after a timeout.
func (c *comparisonAPI) comparisonMember(w http.ResponseWriter, id string, deleting bool) (cluster.Member, bool) {
	_, member, err := c.comparisonNode(id)
	switch {
	case err == nil:
	case errors.Is(err, comparison.ErrNotFound):
		fail(w, 404, "Comparison not found")
		return member, false
	case errors.Is(err, errComparisonNodeRemoved):
		if err := c.forget(id); err != nil {
			fail(w, 500, "Cannot save comparison")
		} else if deleting {
			writeJSON(w, 200, map[string]bool{"success": true})
		} else {
			fail(w, 404, "Comparison not found")
		}
		return member, false
	default:
		fail(w, 500, "Cannot read comparison")
		return member, false
	}
	online, err := c.comparisonOnline(member)
	if err != nil {
		fail(w, 500, "Cannot read Agent state")
		return member, false
	}
	if !online {
		fail(w, 503, "Agent is offline")
		return member, false
	}
	return member, true
}

// comparisonOnline reports whether member has polled recently enough to
// answer now; requests to other members would only wait for a timeout.
func (c *comparisonAPI) comparisonOnline(member cluster.Member) (bool, error) {
	online, err := c.server.onlineAgents()
	if err != nil {
		return false, err
	}
	for _, candidate := range online {
		if candidate.Session() == member.Session() {
			return true, nil
		}
	}
	return false, nil
}

// agentUpdating reports a command refused because the Agent is being
// updated, by the Agent or by the Master before sending it. Nothing started.
func agentUpdating(err error) bool {
	var problem *AgentError
	return errors.As(err, &problem) && problem.Status == 409 && problem.Message == "Agent update is in progress"
}

// comparisonGone reports the Agent's answer for a comparison it deleted or
// expired. Other 404 replies, such as an unknown operation, are not.
func comparisonGone(err error) bool {
	var problem *AgentError
	return errors.As(err, &problem) && problem.Status == 404 && problem.Message == "Comparison not found"
}

// agentFailed replies to a failed Agent call about comparison id. A deleted
// or expired result also removes its assignment; every other failure,
// including other 404 replies such as an unknown operation, keeps it for a
// later attempt and is reported as a gateway error.
func (c *comparisonAPI) agentFailed(w http.ResponseWriter, id string, err error) {
	if comparisonGone(err) {
		if forgetErr := c.forget(id); forgetErr != nil {
			fail(w, 500, "Cannot save comparison")
			return
		}
		fail(w, 404, "Comparison not found")
		return
	}
	if comparisonNodeSilent(err) {
		// Not a gateway failure the operator can act on: the node went away
		// after its last poll. Its results stay there until it reconnects.
		fail(w, 503, "The node holding this comparison did not respond; its results stay on that node and load again when it reconnects")
		return
	}
	fail(w, 502, err.Error())
}

// comparisonNodeSilent reports an Agent call that got no answer: the node
// stopped polling, lost its enrollment session, or never took the command.
func comparisonNodeSilent(err error) bool {
	var problem *AgentError
	if errors.As(err, &problem) {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, agenttransport.ErrSession) || errors.Is(err, agenttransport.ErrNotDelivered)
}

// comparisonProgressReply prepares an Agent's Progress for a client: the
// fixed English sentence is localized and results written by an Agent before
// comparison.Protocol name the row limit that truncated them.
func comparisonProgressReply(w http.ResponseWriter, p *comparison.Progress) {
	p.Error = localize(w, p.Error)
	comparisonTruncation(p)
}

func comparisonTruncation(p *comparison.Progress) {
	if p.RowsTruncated && p.TruncatedReason == "" {
		p.TruncatedReason = comparison.TruncatedLegacyLimit
	}
}

func comparisonPageReply(w http.ResponseWriter, page *comparison.Page) {
	comparisonProgressReply(w, &page.Progress)
	for i := range page.Items {
		page.Items[i].Error = localize(w, page.Items[i].Error)
	}
}

// comparisonStatuses parses a comma-separated result status filter. It
// returns the statuses in comparison.RowStatuses order without duplicates,
// or nil for no filter.
func comparisonStatuses(values []string) ([]string, bool) {
	selected := map[string]bool{}
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.ToUpper(strings.TrimSpace(item))
			if item == "" {
				continue
			}
			if !slices.Contains(comparison.RowStatuses, item) {
				return nil, false
			}
			selected[item] = true
		}
	}
	var statuses []string
	for _, status := range comparison.RowStatuses {
		if selected[status] {
			statuses = append(statuses, status)
		}
	}
	return statuses, true
}

func comparisonStatusSet(statuses []string) map[string]bool {
	if len(statuses) == 0 {
		return nil
	}
	set := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		set[status] = true
	}
	return set
}

// validComparisonPage checks the cursor contract of a page read after
// `after`: row numbers ascend past it and NextAfter, when set, moves forward
// and is at least the last returned row.
func validComparisonPage(page comparison.Page, after int64) bool {
	last := after
	for _, row := range page.Items {
		if row.Number <= last {
			return false
		}
		last = row.Number
	}
	return page.NextAfter == 0 || page.NextAfter > after && page.NextAfter >= last
}

// fetchComparisonRows reads stored rows after query.After: comparison_rows
// from an Agent with comparison.Protocol, otherwise comparison_get, whose
// rows the caller filters. *protocol drops to 0 when the Agent rejects
// comparison_rows as unknown, for instance after a downgrade.
func (c *comparisonAPI) fetchComparisonRows(ctx context.Context, session agenttransport.Session, protocol *int, query comparison.RowsQuery, timeout time.Duration) (comparison.Page, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reply comparison.Page
	if *protocol >= comparison.Protocol {
		err := c.server.agentCall(callCtx, session, "comparison_rows", false, query, &reply)
		if !comparisonUnsupported(err) {
			return reply, err
		}
		c.protocols.forget(session)
		*protocol = 0
		reply = comparison.Page{}
	}
	limit := comparison.MaxLegacyPageRows
	if len(query.Statuses) == 0 {
		limit = min(limit, query.Limit)
	}
	err := c.server.agentCall(callCtx, session, "comparison_get", false, map[string]any{"id": query.ID, "after": query.After, "limit": limit}, &reply)
	return reply, err
}

func (c *comparisonAPI) page(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	after, limit := int64(0), 50
	var err error
	if value := query.Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil || after < 0 {
			fail(w, 400, "Invalid comparison cursor")
			return
		}
	}
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > comparison.MaxPageRows {
			fail(w, 400, "Comparison page size must be between 1 and 1000")
			return
		}
	}
	statuses, ok := comparisonStatuses(query["status"])
	if !ok {
		fail(w, 400, "Unknown comparison result status")
		return
	}
	id := r.PathValue("id")
	member, ok := c.comparisonMember(w, id, false)
	if !ok {
		return
	}
	page, err := c.collectPage(r.Context(), member.Session(), id, after, limit, statuses)
	if err != nil {
		c.agentFailed(w, id, err)
		return
	}
	comparisonPageReply(w, &page)
	writeJSON(w, 200, page)
}

// collectPage reads up to limit stored rows after `after` whose status is in
// statuses (all when empty). While a filtered page is short and stored rows
// remain, it continues from NextAfter until comparisonPageBudget is spent.
func (c *comparisonAPI) collectPage(ctx context.Context, session agenttransport.Session, id string, after int64, limit int, statuses []string) (comparison.Page, error) {
	protocol, err := c.comparisonProtocol(ctx, session)
	if err != nil {
		return comparison.Page{}, err
	}
	filter := comparisonStatusSet(statuses)
	deadline := time.Now().Add(comparisonPageBudget)
	result := comparison.Page{Items: []comparison.Row{}}
	cursor := after
	for {
		reply, err := c.fetchComparisonRows(ctx, session, &protocol, comparison.RowsQuery{ID: id, After: cursor, Limit: limit - len(result.Items), Statuses: statuses, MaxBytes: comparison.MaxPageBytes}, comparisonCallTimeout)
		if err != nil {
			return result, err
		}
		if !validComparisonPage(reply, cursor) {
			return result, errComparisonPage
		}
		result.Progress, result.NextAfter = reply.Progress, reply.NextAfter
		for index, row := range reply.Items {
			if filter != nil && !filter[row.Status] {
				continue
			}
			result.Items = append(result.Items, row)
			if len(result.Items) < limit {
				continue
			}
			if index < len(reply.Items)-1 {
				// Later rows of this reply were not returned; continue after
				// the last one that was.
				result.NextAfter = row.Number
			}
			return result, nil
		}
		if reply.NextAfter == 0 || !time.Now().Before(deadline) {
			return result, nil
		}
		cursor = reply.NextAfter
	}
}

func (c *comparisonAPI) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	member, ok := c.comparisonMember(w, id, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), comparisonCallTimeout)
	defer cancel()
	var progress comparison.Progress
	if err := c.server.agentCall(ctx, member.Session(), "comparison_cancel", true, map[string]any{"id": id}, &progress); err != nil {
		c.agentFailed(w, id, err)
		return
	}
	comparisonProgressReply(w, &progress)
	writeJSON(w, 200, progress)
}

func (c *comparisonAPI) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	member, ok := c.comparisonMember(w, id, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), comparisonCallTimeout)
	defer cancel()
	err := c.server.agentCall(ctx, member.Session(), "comparison_delete", true, map[string]any{"id": id}, nil)
	var problem *AgentError
	switch {
	case err == nil:
	case errors.As(err, &problem) && problem.Status == 409:
		fail(w, 409, problem.Message)
		return
	case errors.As(err, &problem) && problem.Status == 429:
		// Agents before comparison.Protocol call their own running
		// comparison "another comparison".
		fail(w, 409, "Comparison is running; cancel it before deleting")
		return
	default:
		c.agentFailed(w, id, err)
		return
	}
	if err := c.forget(id); err != nil {
		fail(w, 500, "Cannot save comparison")
		return
	}
	writeJSON(w, 200, map[string]bool{"success": true})
}

func (c *comparisonAPI) resume(w http.ResponseWriter, r *http.Request) {
	s := c.server
	id := r.PathValue("id")
	member, ok := c.comparisonMember(w, id, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	session := member.Session()
	protocol, err := c.comparisonProtocol(ctx, session)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	if protocol < comparison.Protocol {
		fail(w, 409, "Update this node to resume comparisons")
		return
	}
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		fail(w, 500, "Cannot read storage connections")
		return
	}
	// The Agent resumes with the connections it has now; send the current
	// ones and name their revision so it refuses any other.
	if err := s.syncStorage(ctx, member, snapshot); err != nil {
		fail(w, 502, redactStorageError(err, snapshot.Connections).Error())
		return
	}
	var progress comparison.Progress
	err = s.agentCall(ctx, session, "comparison_resume", true, comparison.ResumeRequest{ID: id, StorageRevision: snapshot.Revision}, &progress)
	var problem *AgentError
	switch {
	case err == nil:
	case comparisonUnsupported(err):
		c.protocols.forget(session)
		fail(w, 409, "Update this node to resume comparisons")
		return
	case errors.As(err, &problem) && problem.Status == 429:
		fail(w, 409, "Another comparison is running on this node")
		return
	case errors.As(err, &problem) && problem.Status == 409:
		message := problem.Message
		if message == storageconfig.ErrStaleRevision.Error() {
			message = "Storage connections changed; resume the comparison again"
		}
		fail(w, 409, message)
		return
	default:
		c.agentFailed(w, id, err)
		return
	}
	comparisonProgressReply(w, &progress)
	writeJSON(w, 202, progress)
}

// comparisonListItem is a list entry: an Agent's Progress, or for an
// unreachable node an UNAVAILABLE entry built from the assignment.
type comparisonListItem struct {
	comparison.Progress
	// These shadow the embedded times so that an UNAVAILABLE entry of an
	// assignment without a recorded creation time omits them instead of
	// presenting year 1.
	StartedAt time.Time `json:"startedAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	NodeID    string    `json:"nodeId"`
	NodeName  string    `json:"nodeName"`
}

type comparisonUnavailableNode struct {
	NodeID   string `json:"nodeId"`
	NodeName string `json:"nodeName"`
	Count    int    `json:"count"`
}

const comparisonUnavailable = "UNAVAILABLE"

// comparisonNodeListing is one node's part of a list request.
type comparisonNodeListing struct {
	member      cluster.Member
	items       []comparison.Progress
	unavailable []comparisonAssignment
	forget      []string
	// adopt are listed comparisons that have no assignment, for instance
	// after the Master database was restored from an older backup.
	adopt []comparison.Progress
}

func (c *comparisonAPI) assignments() ([]comparisonAssignment, error) {
	var all []comparisonAssignment
	after := ""
	for {
		count := 0
		err := c.server.store.View(func(tx *state.Tx) error {
			return tx.Scan(comparisonAssignmentsBucket, "", after, 256, func(key string, raw json.RawMessage) error {
				var a comparisonAssignment
				if err := json.Unmarshal(raw, &a); err != nil {
					return err
				}
				all = append(all, a)
				after, count = key, count+1
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
		if count < 256 {
			return all, nil
		}
	}
}

// list reconciles the Master's assignments with the Agents that keep the
// results. Assignments an Agent no longer has, and those of removed nodes,
// are forgotten; those of offline or unreachable nodes are listed as
// UNAVAILABLE and kept. Comparisons an Agent lists without an assignment are
// adopted, so that they can be watched, canceled and deleted.
func (c *comparisonAPI) list(w http.ResponseWriter, r *http.Request) {
	s := c.server
	listedAt := c.now()
	assignments, err := c.assignments()
	if err != nil {
		fail(w, 500, "Cannot read comparisons")
		return
	}
	members, err := s.agents.registry.Members()
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	onlineMembers, err := s.onlineAgents()
	if err != nil {
		fail(w, 500, "Cannot read Agent state")
		return
	}
	online := map[agenttransport.Session]bool{}
	for _, member := range onlineMembers {
		online[member.Session()] = true
	}
	known := map[string]cluster.Member{}
	for _, member := range members {
		known[member.ID] = member
	}
	groups := map[agenttransport.Session]int{}
	var listings []comparisonNodeListing
	var pending [][]comparisonAssignment
	group := func(member cluster.Member) int {
		index, seen := groups[member.Session()]
		if !seen {
			index = len(listings)
			groups[member.Session()] = index
			listings = append(listings, comparisonNodeListing{member: member})
			pending = append(pending, nil)
		}
		return index
	}
	assigned := make(map[string]bool, len(assignments))
	var forget []string
	for _, a := range assignments {
		assigned[a.Request.ID] = true
		member, exists := known[a.NodeID]
		if !exists {
			// A removed node took its results with it.
			forget = append(forget, a.Request.ID)
			continue
		}
		if member.EnrollmentID != a.EnrollmentID {
			// The node was enrolled again with the same data directory.
			if a, err = c.rebind(a, member); err != nil {
				if errors.Is(err, comparison.ErrNotFound) || errors.Is(err, errComparisonNodeRemoved) {
					continue
				}
				fail(w, 500, "Cannot save comparison")
				return
			}
		}
		index := group(member)
		pending[index] = append(pending[index], a)
	}
	// Online nodes without assignments may still hold comparisons to adopt.
	for _, member := range onlineMembers {
		if _, exists := known[member.ID]; exists {
			group(member)
		}
	}
	slots := make(chan struct{}, 16)
	var running sync.WaitGroup
	for i := range listings {
		running.Add(1)
		go func() {
			defer running.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			member := listings[i].member
			listings[i] = c.listNode(r.Context(), member, online[member.Session()], pending[i], assigned, listedAt)
		}()
	}
	running.Wait()
	displays := s.nodeDisplaySnapshot()
	items := []comparisonListItem{}
	unavailable := []comparisonUnavailableNode{}
	adopted := map[string]bool{}
	for _, listing := range listings {
		member := listing.member
		name := displays.lookup(member.ID, member.Address).DisplayLabel
		for _, progress := range listing.items {
			comparisonProgressReply(w, &progress)
			items = append(items, comparisonListItem{Progress: progress, StartedAt: progress.StartedAt, UpdatedAt: progress.UpdatedAt, NodeID: member.ID, NodeName: name})
		}
		for _, progress := range listing.adopt {
			if adopted[progress.ID] {
				continue
			}
			created, err := c.adopt(progress, member)
			if err != nil {
				fail(w, 500, "Cannot save comparison")
				return
			}
			if !created {
				// A start recorded it meanwhile; the next list shows it.
				continue
			}
			adopted[progress.ID] = true
			comparisonProgressReply(w, &progress)
			items = append(items, comparisonListItem{Progress: progress, StartedAt: progress.StartedAt, UpdatedAt: progress.UpdatedAt, NodeID: member.ID, NodeName: name})
		}
		for _, a := range listing.unavailable {
			progress := comparison.Progress{Request: a.Request, Status: comparisonUnavailable, StartedAt: a.CreatedAt}
			items = append(items, comparisonListItem{Progress: progress, StartedAt: a.CreatedAt, NodeID: member.ID, NodeName: name})
		}
		if len(listing.unavailable) > 0 {
			unavailable = append(unavailable, comparisonUnavailableNode{NodeID: member.ID, NodeName: name, Count: len(listing.unavailable)})
		}
		forget = append(forget, listing.forget...)
	}
	if len(forget) > 0 {
		if err := c.forget(forget...); err != nil {
			fail(w, 500, "Cannot save comparison")
			return
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].StartedAt.After(items[j].StartedAt)
		}
		return items[i].ID < items[j].ID
	})
	sort.Slice(unavailable, func(i, j int) bool {
		if unavailable[i].NodeName != unavailable[j].NodeName {
			return unavailable[i].NodeName < unavailable[j].NodeName
		}
		return unavailable[i].NodeID < unavailable[j].NodeID
	})
	writeJSON(w, 200, map[string]any{"items": items, "complete": len(unavailable) == 0, "unavailableNodes": unavailable})
}

// adopt records an assignment for a comparison that member listed without
// one. It reports false when an assignment exists by now.
func (c *comparisonAPI) adopt(progress comparison.Progress, member cluster.Member) (bool, error) {
	created := false
	err := c.server.store.Update(func(tx *state.Tx) error {
		found, err := tx.Get(comparisonAssignmentsBucket, progress.ID, &comparisonAssignment{})
		if err != nil || found {
			return err
		}
		created = true
		return tx.Put(comparisonAssignmentsBucket, progress.ID, comparisonAssignment{Request: progress.Request, NodeID: member.ID, EnrollmentID: member.EnrollmentID, CreatedAt: progress.StartedAt.UTC()})
	})
	return created, err
}

// listNode asks one node about its assignments: comparison_list once from an
// Agent with comparison.Protocol, otherwise comparison_get per assignment.
// Comparisons that comparison_list returns and that are not in assigned are
// returned for adoption.
func (c *comparisonAPI) listNode(parent context.Context, member cluster.Member, online bool, assignments []comparisonAssignment, assigned map[string]bool, listedAt time.Time) comparisonNodeListing {
	result := comparisonNodeListing{member: member}
	if !online {
		result.unavailable = assignments
		return result
	}
	// An assignment the Agent does not know may belong to a start that is
	// still in flight; only an older one is known to be gone.
	forgettable := func(a comparisonAssignment) bool {
		return a.CreatedAt.IsZero() || listedAt.Sub(a.CreatedAt) >= comparisonForgetGrace
	}
	ctx, cancel := context.WithTimeout(parent, comparisonListTimeout)
	defer cancel()
	session := member.Session()
	protocol, err := c.comparisonProtocol(ctx, session)
	if err != nil {
		result.unavailable = assignments
		return result
	}
	if protocol >= comparison.Protocol {
		var reply comparison.List
		err := c.server.agentCall(ctx, session, "comparison_list", false, struct{}{}, &reply)
		if err == nil {
			listed := make(map[string]comparison.Progress, len(reply.Items))
			for _, progress := range reply.Items {
				listed[progress.ID] = progress
				if progress.ID != "" && !assigned[progress.ID] {
					result.adopt = append(result.adopt, progress)
				}
			}
			for _, a := range assignments {
				if progress, ok := listed[a.Request.ID]; ok {
					result.items = append(result.items, progress)
				} else if forgettable(a) {
					result.forget = append(result.forget, a.Request.ID)
				}
			}
			return result
		}
		if !comparisonUnsupported(err) {
			result.unavailable = assignments
			return result
		}
		c.protocols.forget(session)
	}
	type outcome struct {
		progress comparison.Progress
		err      error
	}
	outcomes := make([]outcome, len(assignments))
	slots := make(chan struct{}, 4)
	var group sync.WaitGroup
	for i, a := range assignments {
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				outcomes[i].err = ctx.Err()
				return
			}
			var page comparison.Page
			err := c.server.agentCall(ctx, session, "comparison_get", false, map[string]any{"id": a.Request.ID, "after": 0, "limit": 1}, &page)
			outcomes[i] = outcome{page.Progress, err}
		}()
	}
	group.Wait()
	for i, o := range outcomes {
		switch {
		case o.err == nil:
			result.items = append(result.items, o.progress)
		case comparisonGone(o.err):
			if forgettable(assignments[i]) {
				result.forget = append(result.forget, assignments[i].Request.ID)
			}
		default:
			result.unavailable = append(result.unavailable, assignments[i])
		}
	}
	return result
}
