package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
)

// scriptedPolls answers polls from a script: nil is an empty poll, any other
// error a failed one.
type scriptedPolls struct{ script []error }

func (s *scriptedPolls) Poll(context.Context) (agenttransport.Command, error) {
	err := s.script[0]
	s.script = s.script[1:]
	if err == nil {
		err = agenttransport.ErrNoCommand
	}
	return agenttransport.Command{}, err
}
func (s *scriptedPolls) Submit(context.Context, agenttransport.Result) error { return nil }

// The service log shows the Master connection once per change: established,
// lost (with the cause, without a query) and restored (with the downtime).
func TestMasterConnectionChangesAreLoggedOnce(t *testing.T) {
	lost := errors.New(`Get "https://master:8443/api/agent/poll?wait=0": dial tcp 10.0.0.1:8443: connect: connection refused`)
	polls := &scriptedPolls{script: []error{nil, nil, lost, lost, lost, nil, nil}}
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	var lines []string
	transport := &readyTransport{Transport: polls, master: "https://master:8443", now: func() time.Time { return now }, logf: func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	for range len(polls.script) {
		_, _ = transport.Poll(context.Background())
		now = now.Add(20 * time.Second)
	}
	want := []string{
		"Connected to the Master at https://master:8443",
		`Connection to the Master at https://master:8443 lost, retrying: Get "https://master:8443/api/agent/poll?…": dial tcp 10.0.0.1:8443: connect: connection refused`,
		"Connection to the Master at https://master:8443 restored after 1m0s",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("service log:\n%s", strings.Join(lines, "\n"))
	}
	// A first poll that fails reports the failure; a stopping Agent's
	// cancelled poll reports nothing.
	lines = nil
	transport = &readyTransport{Transport: &scriptedPolls{script: []error{lost, nil}}, master: "https://master:8443", now: time.Now, logf: transport.logf}
	_, _ = transport.Poll(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = transport.Poll(ctx)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "Connection to the Master at https://master:8443 lost") {
		t.Fatalf("service log:\n%s", strings.Join(lines, "\n"))
	}
}
