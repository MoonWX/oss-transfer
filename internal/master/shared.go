package master

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// A proof is tied to every participating Agent's current process, not merely
// its long-lived enrollment. An Agent restart must recheck its mounted root.
func (t *taskRuntime) ensureSharedSource(ctx context.Context, p task.Parent) (resultErr error) {
	if !p.Spec.SharedLocalSource {
		return nil
	}
	proofKey := fmt.Sprint(p.ID)
	stamps := []any{p.Generation, p.Spec.Nodes}
	for _, node := range p.Spec.Nodes {
		var capabilities struct {
			BootID string `json:"bootId"`
		}
		if err := t.server.agentCall(ctx, nodeSession(node), "capabilities", false, nil, &capabilities); err != nil {
			return err
		}
		if capabilities.BootID == "" {
			return errors.New("Agent does not report its current process identity")
		}
		stamps = append(stamps, capabilities.BootID)
	}
	encoded, _ := json.Marshal(stamps)
	sum := sha256.Sum256(encoded)
	stamp := hex.EncodeToString(sum[:])
	t.mu.Lock()
	cached := t.sharedProofs[p.ID] == stamp
	t.mu.Unlock()
	if cached {
		return nil
	}
	var probe transferstore.SharedProbe
	if err := t.server.store.Update(func(tx *state.Tx) error {
		found, err := tx.Get("master-shared-probes", proofKey, &probe)
		if err != nil {
			return err
		}
		if found {
			return nil
		}
		probe, err = transferstore.NewSharedProbe()
		if err != nil {
			return err
		}
		return tx.Put("master-shared-probes", proofKey, probe)
	}); err != nil {
		return err
	}
	request := func(node task.Node) map[string]any {
		return map[string]any{"parentId": p.ID, "generation": p.Generation, "root": node.LocalRoot, "probe": probe}
	}
	origin := p.Spec.Nodes[0]
	// The marker identity is durable before creation, so retries after a lost
	// response or Master crash can verify or remove that exact marker.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := t.server.agentCall(cleanupCtx, nodeSession(origin), "shared_probe_cleanup", true, request(origin), nil)
		if err != nil && resultErr == nil {
			resultErr = fmt.Errorf("shared source probe cleanup: %w", err)
		}
		if resultErr == nil {
			t.mu.Lock()
			t.sharedProofs[p.ID] = stamp
			t.mu.Unlock()
		}
	}()
	if err := t.server.agentCall(ctx, nodeSession(origin), "shared_probe_ensure", true, request(origin), nil); err != nil {
		return fmt.Errorf("cannot verify shared source directory: %w", err)
	}
	for _, node := range p.Spec.Nodes {
		if err := t.server.agentCall(ctx, nodeSession(node), "shared_probe_verify", true, request(node), nil); err != nil {
			name := t.server.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
			return fmt.Errorf("Agent %s does not expose the same source directory: %w", name, err)
		}
	}
	return nil
}
