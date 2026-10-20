package master

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type agentControl struct {
	// Enrollment changes must commit and update the broker in the same order.
	// Authenticating a request never activates an enrollment.
	mu            sync.Mutex
	registry      *cluster.Registry
	broker        *agenttransport.Broker
	lastSeen      map[string]time.Time
	remoteAddress map[string]string
}

// Close releases long polls and pending commands before HTTP shutdown. The
// caller owns the database and closes it after all HTTP handlers have exited.
func (s *Server) Close() error {
	var err error
	if s.agents != nil {
		err = s.agents.broker.Close()
	}
	if s.tasks != nil {
		s.tasks.close()
	}
	return err
}

func installAgentRoutes(s *Server) error {
	a := &agentControl{
		registry:      cluster.New(s.store, s.options.Now),
		broker:        agenttransport.NewBroker(agenttransport.BrokerOptions{}),
		lastSeen:      make(map[string]time.Time),
		remoteAddress: make(map[string]string),
	}
	members, err := a.registry.Members()
	if err != nil {
		return err
	}
	for _, member := range members {
		if err := a.broker.Activate(member.Session()); err != nil {
			_ = a.broker.Close()
			return err
		}
	}
	s.agents = a
	s.mux.HandleFunc("POST /api/master/agents/invitations", s.adminOnly(s.createAgentInvitation))
	s.mux.HandleFunc("GET /api/master/nodes", s.adminOnly(s.listAgentNodes))
	s.mux.HandleFunc("GET /api/master/network", s.adminOnly(s.networkSettings))
	s.mux.HandleFunc("PUT /api/master/network", s.adminOnly(s.updateNetworkSettings))
	s.mux.HandleFunc("GET /api/master/nodes/{address}/details", s.adminOnly(s.agentNodeDetails))
	s.mux.HandleFunc("GET /api/master/local-nodes", s.adminOnly(s.localNodesWithSettings))
	s.mux.HandleFunc("DELETE /api/master/nodes/{address}", s.adminOnly(s.removeAgentNode))
	// Static workers are not a registration mechanism for the Go Agent.
	s.mux.HandleFunc("POST /api/master/nodes", s.adminOnly(agentNodeEditingUnavailable))
	s.mux.HandleFunc("PUT /api/master/nodes/{address}", s.adminOnly(s.updateAgentNode))
	s.mux.HandleFunc("POST /api/agents/join", s.joinAgent)
	s.mux.HandleFunc("GET /api/agents/bootstrap/{token}", s.agentBootstrap)
	transport := agenttransport.HTTPHandler{Broker: a.broker, Authenticate: s.authenticateAgent}
	s.mux.Handle(agenttransport.PollPath, transport)
	s.mux.Handle(agenttransport.ResultPath, transport)
	return nil
}

func (s *Server) localNodes(w http.ResponseWriter, r *http.Request) {
	members, err := s.agents.registry.Members()
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	online, err := s.onlineAgents()
	if err != nil {
		fail(w, 500, "Cannot read Agent connection state")
		return
	}
	seen := map[string]bool{}
	for _, member := range online {
		seen[member.ID] = true
	}
	nodes := make([]map[string]any, 0, len(members))
	for _, member := range members {
		nodes = append(nodes, map[string]any{"executionNodeIp": member.Address, "reachable": seen[member.ID], "localTransferSupported": true, "agent": true})
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"nodes": nodes}})
}

func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			fail(w, http.StatusUnauthorized, "Sign in required")
			return
		}
		user, valid, err := s.authenticated(r)
		if err != nil {
			fail(w, http.StatusInternalServerError, "Cannot read session")
			return
		}
		if !valid {
			fail(w, http.StatusUnauthorized, "Sign in required")
			return
		}
		if user.Role != "admin" {
			fail(w, http.StatusForbidden, "Administrator access required")
			return
		}
		next(w, r)
	}
}

func (s *Server) configuration() (Configuration, error) {
	var config Configuration
	err := s.store.View(func(tx *state.Tx) error {
		_, err := tx.Get("system", "configuration", &config)
		return err
	})
	return config, err
}

func (s *Server) createAgentInvitation(w http.ResponseWriter, r *http.Request) {
	var change struct {
		Address string `json:"address"`
		Label   string `json:"label"`
	}
	if !readJSON(w, r, &change) {
		return
	}
	config, err := s.configuration()
	if err != nil {
		fail(w, 500, "Cannot read Master address")
		return
	}
	invite, err := s.agents.registry.Invite(strings.TrimSpace(change.Address), strings.TrimSpace(change.Label))
	if err != nil {
		fail(w, 400, "Cannot create Agent invitation")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": s.agentInvitationData(config.agentURL(), invite)})
}

func (s *Server) joinAgent(w http.ResponseWriter, r *http.Request) {
	var request cluster.JoinRequest
	if !readJSON(w, r, &request) {
		return
	}
	a := s.agents
	a.mu.Lock()
	defer a.mu.Unlock()
	member, err := a.registry.Join(request)
	switch {
	case errors.Is(err, cluster.ErrInvitation):
		fail(w, 403, "Invalid or expired Agent invitation")
		return
	case errors.Is(err, cluster.ErrConflict):
		fail(w, 409, "Agent identity or address is already registered")
		return
	case err != nil:
		fail(w, 500, "Cannot register Agent")
		return
	}
	if err := a.broker.Activate(member.Session()); err != nil {
		// Join is durable and idempotent for this identity, so retrying is safe.
		fail(w, 503, "Agent command service is unavailable; retry joining")
		return
	}
	a.lastSeen[member.ID] = s.options.Now().UTC()
	a.remoteAddress[member.ID] = connectionHost(r.RemoteAddr)
	writeJSON(w, 200, map[string]any{"success": true, "data": member, "session": member.Session()})
}

func (s *Server) authenticateAgent(r *http.Request) (agenttransport.Session, error) {
	a := s.agents
	a.mu.Lock()
	defer a.mu.Unlock()
	session, err := a.registry.Authenticate(r.Header.Get("X-Agent-Key"))
	if err == nil {
		a.lastSeen[session.NodeID] = s.options.Now().UTC()
		a.remoteAddress[session.NodeID] = connectionHost(r.RemoteAddr)
	}
	return session, err
}

func connectionHost(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

type registeredAgentNode struct {
	ID       string     `json:"id"`
	Address  string     `json:"address"`
	Label    string     `json:"label"`
	Master   bool       `json:"master"`
	Agent    bool       `json:"agent"`
	Online   bool       `json:"online"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

func (s *Server) agentNodeData() (map[string]any, error) {
	config, err := s.configuration()
	if err != nil {
		return nil, err
	}
	a := s.agents
	a.mu.Lock()
	defer a.mu.Unlock()
	members, err := a.registry.Members()
	if err != nil {
		return nil, err
	}
	nodes := make([]registeredAgentNode, 0, len(members))
	for _, member := range members {
		node := registeredAgentNode{ID: member.ID, Address: member.Address, Label: member.Label, Agent: true}
		if seen, ok := a.lastSeen[member.ID]; ok {
			node.LastSeen = &seen
			node.Online = s.options.Now().Sub(seen) < 75*time.Second
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Address < nodes[j].Address })
	u, err := url.Parse(config.PublicURL)
	if err != nil {
		return nil, err
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"nodes": nodes, "port": port, "masterWebUrl": config.PublicURL, "masterAgentUrl": config.agentURL()}, nil
}

func (s *Server) listAgentNodes(w http.ResponseWriter, r *http.Request) {
	data, err := s.agentNodeData()
	if err != nil {
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": data})
}

func (s *Server) removeAgentNode(w http.ResponseWriter, r *http.Request) {
	s.topologyMu.Lock()
	defer s.topologyMu.Unlock()
	a := s.agents
	a.mu.Lock()
	members, err := a.registry.Members()
	if err != nil {
		a.mu.Unlock()
		fail(w, 500, "Cannot read registered Agents")
		return
	}
	var found *cluster.Member
	for i := range members {
		if members[i].Address == r.PathValue("address") {
			found = &members[i]
			break
		}
	}
	if found == nil {
		a.mu.Unlock()
		fail(w, 404, "Agent not found")
		return
	}
	if s.tasks != nil {
		after := int64(0)
		for {
			parents, readErr := s.tasks.repo.List(after, 128, true)
			if readErr != nil {
				a.mu.Unlock()
				fail(w, 500, "Cannot check active tasks")
				return
			}
			for _, parent := range parents {
				for _, node := range parent.Spec.Nodes {
					if node.ID == found.ID {
						a.mu.Unlock()
						fail(w, 409, "Stop active tasks before removing this Agent")
						return
					}
				}
			}
			if len(parents) < 128 {
				break
			}
			after = parents[len(parents)-1].ID
		}
	}
	session, err := a.registry.Revoke(found.ID)
	if err == nil {
		// Durable revocation precedes invalidating in-flight commands. A failed
		// response does not grant the old credential another activation path.
		_ = a.broker.Revoke(session)
		delete(a.lastSeen, found.ID)
	}
	a.mu.Unlock()
	if err != nil {
		fail(w, 500, "Cannot revoke Agent")
		return
	}
	s.listAgentNodes(w, r)
}

func agentNodeEditingUnavailable(w http.ResponseWriter, r *http.Request) {
	fail(w, http.StatusNotImplemented, "Use an Agent invitation to add a node; editing registered nodes is not available yet")
}
