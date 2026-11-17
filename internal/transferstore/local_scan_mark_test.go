package transferstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

// markStreamed marks the directory at depth in a saved cursor as streamed, as
// an Agent that streams long directories saves it.
func markStreamed(t *testing.T, value string, depth int) string {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	var saved cursor
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	var frames []localScanFrame
	if err := json.Unmarshal([]byte(saved.Position), &frames); err != nil {
		t.Fatal(err)
	}
	frames[depth].Streamed = true
	position, err := json.Marshal(frames)
	if err != nil {
		t.Fatal(err)
	}
	saved.Position = string(position)
	encoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// A directory marked as streamed keeps its mark in every cursor while it is
// on the path, however often the scan resumes, and its pages say so; once
// the scan has left it, they do not.
func TestLocalScanKeepsTheMarkOfAStreamedDirectory(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 4)...)
	writeFiles(t, root, "big/dir/g0", "big/dir/g1", "z")
	ctx, stop := context.Background(), errors.New("saved")
	options := ListOptions{Limit: 1}
	for {
		var page Page
		if err := local.ScanPages(ctx, options, func(p Page) error { page = p; return stop }); !errors.Is(err, stop) {
			t.Fatal(err)
		}
		if page.Streamed {
			t.Fatal("a cursor without a mark was reported as streamed")
		}
		if options.Cursor = page.NextCursor; len(decodeFrames(t, local, options.Cursor)) == 3 {
			break
		}
	}
	// An Agent that streamed big saved this position inside big/dir.
	options.Cursor = markStreamed(t, options.Cursor, 1)
	marked, unmarked := 0, 0
	for options.Cursor != "" {
		var page Page
		if err := local.ScanPages(ctx, options, func(p Page) error { page = p; return stop }); !errors.Is(err, stop) {
			t.Fatal(err)
		}
		frames := decodeFrames(t, local, page.NextCursor)
		onPath := len(frames) > 1 && frames[1].Name == "big"
		if page.Streamed != onPath || onPath && !frames[1].Streamed || frames[0].Streamed || len(frames) > 2 && frames[2].Streamed {
			t.Fatalf("page %+v under %+v", page, frames)
		}
		if onPath {
			marked++
		} else {
			unmarked++
		}
		options.Cursor = page.NextCursor
	}
	if marked == 0 || unmarked == 0 {
		t.Fatalf("%d pages inside big, %d after it", marked, unmarked)
	}
}

// Another Agent that takes over a scan keeps the mark of the cursor it
// imports.
func TestSharedScanCheckpointKeepsTheStreamedMark(t *testing.T) {
	source, root, saved := sharedImportFixture(t)
	rebound, err := source.ImportScanCursor(ListOptions{Cursor: markStreamed(t, saved, 1)}, root)
	if err != nil {
		t.Fatal(err)
	}
	if frames := decodeFrames(t, source, rebound); len(frames) != 2 || frames[0].Streamed || !frames[1].Streamed {
		t.Fatalf("imported frames %+v", frames)
	}
	var first Page
	stop := errors.New("saved")
	if err := source.ScanPages(context.Background(), ListOptions{Cursor: rebound, Limit: 1}, func(p Page) error { first = p; return stop }); !errors.Is(err, stop) || !first.Streamed {
		t.Fatalf("first page after the import %+v, %v", first, err)
	}
}
