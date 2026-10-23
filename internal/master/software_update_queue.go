package master

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type agentUpdateItem struct {
	NodeID           string    `json:"nodeId"`
	EnrollmentID     string    `json:"enrollmentId"`
	Address          string    `json:"address"`
	Phase            string    `json:"phase"`
	Reason           string    `json:"reason,omitempty"`
	Error            string    `json:"error,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
	StartedAt        time.Time `json:"startedAt,omitempty"`
	PreviousID       string    `json:"previousId,omitempty"`
	UpdateID         string    `json:"updateId,omitempty"`
	UnavailableSince time.Time `json:"unavailableSince,omitempty"`
}

type agentUpdateQueue struct {
	ID        string            `json:"id,omitempty"`
	Version   string            `json:"version,omitempty"`
	Phase     string            `json:"phase"`
	Items     []agentUpdateItem `json:"items"`
	UpdatedAt time.Time         `json:"updatedAt,omitempty"`
}

func (s *Server) softwareQueue() (agentUpdateQueue, error) {
	q := agentUpdateQueue{Phase: "IDLE", Items: []agentUpdateItem{}}
	err := s.store.View(func(tx *state.Tx) error { _, err := tx.Get("software_updates", "agents", &q); return err })
	return q, err
}

func (s *Server) saveSoftwareQueue(q agentUpdateQueue) error {
	q.UpdatedAt = time.Now().UTC()
	return s.store.Update(func(tx *state.Tx) error { return tx.Put("software_updates", "agents", q) })
}

func (s *Server) softwareUpdateBlocksNode(nodeID string) bool {
	q, err := s.softwareQueue()
	if err != nil {
		return true
	}
	for _, item := range q.Items {
		if item.NodeID == nodeID && item.Phase == "UPDATING" {
			return true
		}
	}
	return false
}

func (s *Server) enqueueAgentUpdates(ctx context.Context, version, address string) (agentUpdateQueue, error) {
	s.updates.queueMu.Lock()
	defer s.updates.queueMu.Unlock()
	old, err := s.softwareQueue()
	if err != nil {
		return old, err
	}
	master, status := s.localSoftware()
	if master.Version != version || updateInProgress(status.Phase) {
		return old, errors.New("Update Agents to the current Master version after the Master update finishes")
	}
	if old.Phase == "RUNNING" {
		if old.Version == version {
			for _, item := range old.Items {
				if address == "" || item.Address == address {
					return old, nil
				}
			}
		}
		return old, errors.New("Another Agent update is already in progress")
	}
	members, err := s.agents.registry.Members()
	if err != nil {
		return old, err
	}
	id, err := cluster.Token()
	if err != nil {
		return old, err
	}
	q := agentUpdateQueue{ID: id, Version: version, Phase: "RUNNING", Items: []agentUpdateItem{}}
	for _, member := range members {
		if address != "" && member.Address != address {
			continue
		}
		q.Items = append(q.Items, agentUpdateItem{NodeID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address, Phase: "QUEUED", UpdatedAt: time.Now().UTC()})
	}
	if len(q.Items) == 0 {
		return q, errors.New("No registered Agents match this update")
	}
	if err := ctx.Err(); err != nil {
		return q, err
	}
	if err := s.saveSoftwareQueue(q); err != nil {
		return q, err
	}
	return q, nil
}

func (s *Server) startSoftwareUpdates() {
	s.updates.lifecycle.Lock()
	defer s.updates.lifecycle.Unlock()
	if s.updates.done != nil || s.updates.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.updates.cancel, s.updates.done = cancel, make(chan struct{})
	go func() {
		defer close(s.updates.done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			s.stepSoftwareQueue(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) closeSoftwareUpdates() {
	s.updates.lifecycle.Lock()
	s.updates.closed = true
	cancel, done := s.updates.cancel, s.updates.done
	if cancel != nil {
		cancel()
	}
	s.updates.lifecycle.Unlock()
	if done != nil {
		<-done
	}
}

type agentSoftware struct {
	Info   softwareupdate.Info   `json:"info"`
	Update softwareupdate.Status `json:"update"`
}

// The queue is persisted before dispatch. A lost response is reconciled from
// the Agent's persisted status instead of starting the next node speculatively.
func (s *Server) stepSoftwareQueue(parent context.Context) {
	s.updates.queueMu.Lock()
	defer s.updates.queueMu.Unlock()
	q, err := s.softwareQueue()
	if err != nil || q.Phase != "RUNNING" || parent.Err() != nil {
		return
	}
	master, status := s.localSoftware()
	if updateInProgress(status.Phase) {
		return
	}
	for index := range q.Items {
		item := &q.Items[index]
		if item.Phase != "QUEUED" && item.Phase != "UPDATING" {
			continue
		}
		finish := func(phase, reason, message string) {
			item.Phase, item.Reason, item.Error, item.UpdatedAt = phase, reason, message, time.Now().UTC()
			if phase == "FAILED" {
				q.Phase = "FAILED"
				for i := range q.Items {
					if q.Items[i].Phase == "QUEUED" {
						q.Items[i].Phase, q.Items[i].Reason, q.Items[i].UpdatedAt = "SKIPPED", "PREVIOUS_FAILED", time.Now().UTC()
					}
				}
			}
			_ = s.saveSoftwareQueue(q)
		}
		if master.Version != q.Version {
			finish("FAILED", "MASTER_CHANGED", "The Master version changed; start a new Agent update")
			return
		}
		expired := item.Phase == "UPDATING" && time.Since(item.StartedAt) > time.Hour
		member, err := s.nodeByAddress(item.Address)
		if err != nil || member.ID != item.NodeID || member.EnrollmentID != item.EnrollmentID {
			finish("FAILED", "NODE_CHANGED", "The registered Agent changed; start a new update")
			return
		}
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		var remote agentSoftware
		err = s.agentCall(ctx, member.Session(), "software_info", false, struct{}{}, &remote)
		cancel()
		if parent.Err() != nil {
			return
		}
		if err != nil {
			if item.Phase == "QUEUED" {
				var remoteError *AgentError
				if errors.As(err, &remoteError) {
					finish("SKIPPED", "LEGACY", "Upgrade this Agent once with the installer to enable online updates")
				} else {
					if item.UnavailableSince.IsZero() {
						item.UnavailableSince, item.UpdatedAt, item.Reason = time.Now().UTC(), time.Now().UTC(), "RECONNECTING"
						_ = s.saveSoftwareQueue(q)
					} else if time.Since(item.UnavailableSince) > 30*time.Second {
						finish("SKIPPED", "OFFLINE", "Agent is offline")
					}
				}
			} else if expired {
				finish("FAILED", "TIMEOUT", "Agent update did not finish; inspect its service before retrying")
			}
			return
		}
		item.UnavailableSince, item.Reason = time.Time{}, ""
		currentAttempt := item.UpdateID != "" && item.UpdateID == remote.Update.ID || item.UpdateID == "" && remote.Update.ID != item.PreviousID
		// Master downtime is not evidence that an Agent update failed. Read a
		// persisted terminal helper result before applying the elapsed timeout.
		if item.Phase == "UPDATING" && currentAttempt && remote.Update.Version == q.Version {
			if remote.Update.Phase == "COMPLETED" && remote.Info.Version == q.Version {
				finish("COMPLETED", "", "")
				return
			}
			if remote.Update.Phase == "FAILED" {
				finish("FAILED", "UPDATE_FAILED", remote.Update.Error)
				return
			}
		}
		if expired {
			finish("FAILED", "TIMEOUT", "Agent update did not finish; inspect its service before retrying")
			return
		}
		if remote.Info.Version == q.Version && !updateInProgress(remote.Update.Phase) {
			if item.Phase == "UPDATING" && currentAttempt && remote.Update.Phase == "FAILED" {
				finish("FAILED", "UPDATE_FAILED", remote.Update.Error)
			} else if item.Phase == "QUEUED" || currentAttempt && remote.Update.Phase == "COMPLETED" {
				phase, reason := "COMPLETED", ""
				if item.Phase == "QUEUED" {
					phase, reason = "SKIPPED", "UP_TO_DATE"
				}
				finish(phase, reason, "")
			}
			return
		}
		if item.Phase == "QUEUED" && updateInProgress(remote.Update.Phase) {
			if remote.Update.Version != q.Version {
				finish("SKIPPED", "BUSY", "Another Agent update is already in progress")
				return
			}
			item.Phase, item.StartedAt, item.UpdatedAt = "UPDATING", time.Now().UTC(), time.Now().UTC()
			item.UpdateID = remote.Update.ID
			_ = s.saveSoftwareQueue(q)
			return
		}
		if item.Phase == "UPDATING" {
			if currentAttempt && remote.Update.Version == q.Version && remote.Update.Phase == "FAILED" {
				finish("FAILED", "UPDATE_FAILED", remote.Update.Error)
				return
			}
			if updateInProgress(remote.Update.Phase) {
				return
			}
		} else {
			if !remote.Info.Supported {
				finish("SKIPPED", remote.Info.Reason, remote.Info.Detail)
				return
			}
			if !newerSoftware(q.Version, remote.Info.Version) {
				finish("SKIPPED", "VERSION", "Only older stable Agent releases can be updated")
				return
			}
			item.Phase, item.StartedAt, item.UpdatedAt = "UPDATING", time.Now().UTC(), time.Now().UTC()
			item.PreviousID = remote.Update.ID
			if err := s.saveSoftwareQueue(q); err != nil {
				return
			}
		}
		ctx, cancel = context.WithTimeout(parent, 10*time.Second)
		var result softwareupdate.Status
		err = s.agentCall(ctx, member.Session(), "software_update", true, map[string]string{"version": q.Version}, &result)
		cancel()
		if err != nil {
			var remoteError *AgentError
			if errors.As(err, &remoteError) {
				finish("FAILED", "UPDATE_FAILED", remoteError.Message)
			}
		} else {
			item.UpdateID, item.UpdatedAt = result.ID, time.Now().UTC()
			_ = s.saveSoftwareQueue(q)
		}
		return
	}
	q.Phase = "COMPLETED"
	_ = s.saveSoftwareQueue(q)
}
