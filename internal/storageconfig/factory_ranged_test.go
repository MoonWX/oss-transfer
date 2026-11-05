package storageconfig

import (
	"context"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// LOCAL destinations write large objects in ranges only with the Agent's
// shared memory budget; without one they always stream.
func TestFactoryLocalRangedWritesUseTheNodeMemoryBudget(t *testing.T) {
	budget, err := transferstore.NewUploadMemoryBudget(transferstore.DefaultUploadMemoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		factory *Factory
		want    bool
	}{{&Factory{UploadMemory: budget, PartLender: transferstore.NewPartLender()}, true}, {&Factory{}, false}} {
		store, err := test.factory.OpenEndpoint(context.Background(), execution.Endpoint{Kind: "LOCAL", Root: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		writer, ok := store.(transferstore.RangeWriter)
		if !ok || writer.RangedWrite(128<<20) != test.want || writer.RangedWrite(64<<20) {
			t.Fatalf("ranged selection with memory=%v", test.factory.UploadMemory != nil)
		}
		_ = store.Close()
	}
}
