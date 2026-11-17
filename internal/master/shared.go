package master

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	if p.Spec.SharedCheckSkipped {
		// grantSkippedSharedCheck logs each node that starts unverified.
		skipped, verified := skippedSharedNodes(p, p.Spec.Nodes)
		if err := t.server.grantSkippedSharedCheck(ctx, p, skipped); err != nil {
			return err
		}
		if len(verified) == 0 {
			t.mu.Lock()
			t.sharedProofs[p.ID] = stamp
			t.mu.Unlock()
			return nil
		}
		// The marker below verifies the nodes whose check was not skipped.
		p.Spec.Nodes = verified
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
	var attempted []task.Node
	// The marker identity is durable before creation, so retries after a lost
	// response or Master crash can verify or remove that exact marker. A node
	// that refused the write holds none; its cleanup finds nothing to remove.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, node := range attempted {
			err := t.server.agentCall(cleanupCtx, nodeSession(node), "shared_probe_cleanup", true, request(node), nil)
			if err != nil && resultErr == nil {
				resultErr = fmt.Errorf("shared source probe cleanup: %w", err)
			}
		}
		if resultErr == nil {
			t.mu.Lock()
			t.sharedProofs[p.ID] = stamp
			t.mu.Unlock()
		}
	}()
	// Any node may write the marker, as in the precheck. Only when no node
	// can write the source at all are sampled files compared instead.
	written := false
	for _, node := range p.Spec.Nodes {
		attempted = append(attempted, node)
		err := t.server.agentCall(ctx, nodeSession(node), "shared_probe_ensure", true, request(node), nil)
		if err == nil {
			written = true
			break
		}
		if !unwritableFailure(err) {
			return fmt.Errorf("cannot verify shared source directory: %w", err)
		}
	}
	if !written {
		sample, _, err := t.server.proveSharedFingerprints(ctx, p.Spec.Nodes, p.ID, p.Generation)
		if err != nil {
			return err
		}
		log.Printf("task %d generation %d: no node could write the test file into the shared source (%s); %d sampled files have the same size and first and last 64 KiB on every node instead", p.ID, p.Generation, t.server.nodeLabels(p.Spec.Nodes), len(sample))
		return nil
	}
	for _, node := range p.Spec.Nodes {
		if err := t.server.agentCall(ctx, nodeSession(node), "shared_probe_verify", true, request(node), nil); err != nil {
			name := t.server.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
			return fmt.Errorf("Agent %s does not expose the same source directory: %w", name, err)
		}
	}
	return nil
}
