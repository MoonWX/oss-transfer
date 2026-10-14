package execution

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedLocalFingerprintAllowsOnlyVerifiedModeAndKeepsPortableChecks(t *testing.T) {
	f := newFixture(t, 2)
	source, target, err := f.factory().Open(bg, f.job)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	defer target.Close()
	engine := &Engine{}
	item := f.job.Items[1]
	item.Source.ETag = "local:other-node-device-and-inode"
	if result := engine.copyItem(bg, f.job, item, source, target); result.Status != ItemFailed {
		t.Fatalf("ordinary local task ignored identity: %+v", result)
	}
	job := f.job
	job.SharedLocalSource = true
	if result := engine.copyItem(bg, job, item, source, target); result.Status != ItemSucceeded {
		t.Fatalf("verified cross-node fingerprint rejected: %+v", result)
	}
	for _, change := range []func(*Item){
		func(i *Item) { i.Source.Size++ },
		func(i *Item) { i.Source.ModifiedAt = i.Source.ModifiedAt.Add(time.Second) },
		func(i *Item) { i.Source.ModifiedAt = time.Time{} },
		func(i *Item) { i.Source.ETag = "s3-etag" },
	} {
		changed := item
		change(&changed)
		if result := engine.copyItem(bg, job, changed, source, target); result.Status != ItemFailed {
			t.Fatalf("shared mode ignored source change: %+v", result)
		}
	}
	job.Source.Kind = "S3"
	if result := engine.copyItem(bg, job, item, source, target); result.Status != ItemFailed {
		t.Fatalf("shared flag weakened non-local fingerprint: %+v", result)
	}
}

func TestSharedLocalSourceStillDetectsInFlightMutation(t *testing.T) {
	f := newFixture(t, 2)
	source, target, err := f.factory().Open(bg, f.job)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	defer target.Close()
	item := f.job.Items[1]
	item.Source.ETag = "local:remote-node"
	job := f.job
	job.SharedLocalSource = true
	engine := &Engine{opts: Options{DecorateReader: func(_ context.Context, _ Job, _ Item, input io.ReadCloser) (io.ReadCloser, error) {
		changed := item.Source.ModifiedAt.Add(time.Second)
		return input, os.Chtimes(filepath.Join(f.src, item.Source.Key), changed, changed)
	}}}
	if result := engine.copyItem(bg, job, item, source, target); result.Status != ItemFailed || result.TargetCommitted {
		t.Fatalf("in-flight local source mutation committed: %+v", result)
	}
	if _, err := target.Stat(bg, item.DestinationKey); err == nil {
		t.Fatal("changed source published target")
	}
}
