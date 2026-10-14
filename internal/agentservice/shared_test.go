package agentservice

import (
	"context"
	"errors"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestSharedVerifierRequiresProofAndFreshBoot(t *testing.T) {
	ctx := context.Background()
	root, other := t.TempDir(), t.TempDir()
	probe, err := transferstore.CreateSharedProbe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	verifier := NewSharedVerifier()
	if verifier.Verified(123, 1, root) {
		t.Fatal("unverified root allowed")
	}
	if err := verifier.Verify(ctx, 123, 1, other, probe); err == nil || verifier.Verified(123, 1, other) {
		t.Fatal("unshared root accepted")
	}
	if err := verifier.Verify(ctx, 123, 1, root, probe); err != nil || !verifier.Verified(123, 1, root) {
		t.Fatalf("valid proof rejected: %v", err)
	}
	if verifier.Verified(124, 1, root) || verifier.Verified(123, 2, root) || verifier.Verified(123, 1, other) {
		t.Fatal("proof leaked across parent, generation or root")
	}
	if NewSharedVerifier().Verified(123, 1, root) {
		t.Fatal("new Agent process inherited proof")
	}
	if err := verifier.Verify(ctx, 123, 2, root, probe); err != nil {
		t.Fatal(err)
	}
	if verifier.Verified(123, 1, root) || !verifier.Verified(123, 2, root) {
		t.Fatal("generation transition retained stale proof")
	}
	if err := verifier.Verify(ctx, 123, 1, root, probe); !errors.Is(err, ErrSharedVerificationStale) || !verifier.Verified(123, 2, root) {
		t.Fatalf("stale proof displaced current proof: %v", err)
	}
	if err := transferstore.CleanupSharedProbe(ctx, root, probe); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(ctx, 123, 2, root, probe); err == nil || verifier.Verified(123, 2, root) {
		t.Fatal("failed revalidation retained old proof")
	}
	if err := transferstore.EnsureSharedProbe(ctx, root, probe); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(ctx, 123, 2, root, probe); err != nil {
		t.Fatal(err)
	}
	verifier.Forget(123, 2)
	if verifier.Verified(123, 2, root) {
		t.Fatal("forgotten proof retained")
	}
}

func TestSharedVerifierFactoryBlocksUnverifiedIO(t *testing.T) {
	v := NewSharedVerifier()
	called := false
	base := execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		called = true
		return nil, nil, nil
	})
	job := execution.Job{ParentID: 123, Generation: 1, SharedLocalSource: true, Source: execution.Endpoint{Kind: "LOCAL", Root: t.TempDir()}}
	if _, _, err := v.Factory(base).Open(context.Background(), job); !errors.Is(err, ErrSharedVerificationRequired) || called {
		t.Fatalf("unverified factory invoked I/O: %t %v", called, err)
	}
	job.SharedLocalSource = false
	if _, _, err := v.Factory(base).Open(context.Background(), job); err != nil || !called {
		t.Fatalf("ordinary task blocked: %v", err)
	}
}
