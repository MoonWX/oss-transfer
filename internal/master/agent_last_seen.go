package master

import (
	"encoding/json"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// When each Agent last connected survives a Master restart, so a node that
// stays down across the restart still shows when it was last seen instead of
// "has not connected". Being online does not use it: after a restart an Agent
// counts as online only once it connects to this process (lastSeen).
const lastSeenBucket = "agent-last-seen"

// lastSeenSaveInterval bounds how often a connected Agent's time is written.
// Close saves the latest times, so only a crash loses up to this much.
const lastSeenSaveInterval = time.Minute

type storedLastSeen struct {
	LastSeen time.Time `json:"lastSeen"`
}

// loadLastSeen reads the saved times. An entry that cannot be read is left
// out: it only means a time that is not shown.
func loadLastSeen(store *state.Store, into map[string]time.Time) error {
	return store.View(func(tx *state.Tx) error {
		after := ""
		for {
			count := 0
			if err := tx.Scan(lastSeenBucket, "", after, 256, func(key string, raw json.RawMessage) error {
				var stored storedLastSeen
				if json.Unmarshal(raw, &stored) == nil && !stored.LastSeen.IsZero() {
					into[key] = stored.LastSeen
				}
				after, count = key, count+1
				return nil
			}); err != nil {
				return err
			}
			if count < 256 {
				return nil
			}
		}
	})
}

// noteConnectionLocked records that the Agent id connected at now, and
// reports whether that time is due to be saved. a.mu is held.
func (a *agentControl) noteConnectionLocked(id string, now time.Time) bool {
	a.lastSeen[id] = now
	if now.Sub(a.lastSeenSaved[id]) < lastSeenSaveInterval {
		return false
	}
	if a.lastSeenSaved == nil {
		a.lastSeenSaved = map[string]time.Time{}
	}
	a.lastSeenSaved[id] = now
	return true
}

// lastConnectionLocked is when the Agent id last connected: to this process,
// or as saved before the Master restarted. a.mu is held.
func (a *agentControl) lastConnectionLocked(id string) (time.Time, bool) {
	if seen, ok := a.lastSeen[id]; ok {
		return seen, true
	}
	seen, ok := a.lastSeenSaved[id]
	return seen, ok
}

// saveLastSeen writes the given times; a saved time never moves back.
func (s *Server) saveLastSeen(times map[string]time.Time) error {
	return s.store.Update(func(tx *state.Tx) error {
		for id, seen := range times {
			var stored storedLastSeen
			if found, err := tx.Get(lastSeenBucket, id, &stored); err == nil && found && !seen.After(stored.LastSeen) {
				continue
			}
			if err := tx.Put(lastSeenBucket, id, storedLastSeen{seen}); err != nil {
				return err
			}
		}
		return nil
	})
}

// flushLastSeen saves every time newer than the saved one, when the Master
// stops.
func (s *Server) flushLastSeen() {
	a := s.agents
	a.mu.Lock()
	due := map[string]time.Time{}
	for id, seen := range a.lastSeen {
		if seen.After(a.lastSeenSaved[id]) {
			due[id] = seen
		}
	}
	if len(due) > 0 && a.lastSeenSaved == nil {
		a.lastSeenSaved = map[string]time.Time{}
	}
	for id, seen := range due {
		a.lastSeenSaved[id] = seen
	}
	a.mu.Unlock()
	if len(due) > 0 && s.store != nil {
		_ = s.saveLastSeen(due)
	}
}
