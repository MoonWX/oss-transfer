package master

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
)

// comparisonProtocolTTL bounds how long a positive comparisonProtocol answer
// is reused. An Agent downgraded in place is noticed when the answer expires,
// or earlier when it rejects one of the newer operations.
const comparisonProtocolTTL = time.Minute

// comparisonProtocols remembers Agents that advertised comparison.Protocol or
// later, per enrollment, as rangedDownloadSupport does for ranged downloads.
// Older answers and failed queries are not kept: an Agent upgraded in place is
// asked again on its next use, and a slow query that started before an
// upgrade cannot replace a newer positive answer with a negative one.
type comparisonProtocols struct {
	mu      sync.Mutex
	answers map[agenttransport.Session]comparisonProtocolAnswer
}

type comparisonProtocolAnswer struct {
	protocol int
	until    time.Time
}

func (c *comparisonProtocols) get(ctx context.Context, now func() time.Time, session agenttransport.Session, query func(context.Context, agenttransport.Session) (int, error)) (int, error) {
	c.mu.Lock()
	answer, known := c.answers[session]
	c.mu.Unlock()
	if known && now().Before(answer.until) {
		return answer.protocol, nil
	}
	protocol, err := query(ctx, session)
	if err != nil {
		return 0, err
	}
	if protocol < comparison.Protocol {
		return protocol, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answers == nil {
		c.answers = map[agenttransport.Session]comparisonProtocolAnswer{}
	}
	c.answers[session] = comparisonProtocolAnswer{protocol: protocol, until: now().Add(comparisonProtocolTTL)}
	return protocol, nil
}

// forget drops a remembered answer after the Agent rejected a newer
// operation, so the next request asks it again.
func (c *comparisonProtocols) forget(session agenttransport.Session) {
	c.mu.Lock()
	delete(c.answers, session)
	c.mu.Unlock()
}

// comparisonProtocol returns the comparison protocol of the Agent behind
// session: 0 for an Agent that does not advertise one.
func (c *comparisonAPI) comparisonProtocol(ctx context.Context, session agenttransport.Session) (int, error) {
	return c.protocols.get(ctx, c.server.options.Now, session, c.queryComparisonProtocol)
}

func (c *comparisonAPI) queryComparisonProtocol(ctx context.Context, session agenttransport.Session) (int, error) {
	var caps struct {
		ComparisonProtocol int `json:"comparisonProtocol"`
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := c.server.agentCall(callCtx, session, "capabilities", false, nil, &caps)
	if comparisonUnsupported(err) {
		// An executor that cannot describe itself predates the protocol.
		return 0, nil
	}
	return caps.ComparisonProtocol, err
}

// comparisonUnsupported reports an Agent that does not know the operation it
// was sent: an older release or an Agent built without comparisons.
func comparisonUnsupported(err error) bool {
	var problem *AgentError
	if !errors.As(err, &problem) || (problem.Status != 400 && problem.Status != 404 && problem.Status != 501) {
		return false
	}
	message := strings.ToLower(problem.Message)
	return strings.Contains(message, "unknown operation") || strings.Contains(message, "unknown_operation") || strings.Contains(message, "unsupported operation") || strings.Contains(message, "unsupported_operation")
}
