package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/servicehost"
)

// The first start of a Master prints the initial password before "Master
// listening on": oss-transfer-run.sh reads the log once that line is there,
// and only then does the log hold the password of that start.
func TestFirstStartPrintsTheInitialPasswordBeforeListening(t *testing.T) {
	assets := t.TempDir()
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSS_TRANSFER_WEB_DIR", assets)
	config := servicehost.Config{ServiceName: "oss-transfer-master", DataDirectory: t.TempDir(), Listen: "127.0.0.1:0"}

	previous := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	os.Stdout = writer
	output := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		output <- string(data)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The Master reports itself ready after its startup lines; stop it then.
	err = serve(ctx, config, nil, func() error { cancel(); return nil })
	os.Stdout = previous
	_ = writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	printed := <-output
	password := strings.Index(printed, "\nInitial password: ")
	administrator := strings.Index(printed, "Initial administrator: admin\n")
	listening := strings.Index(printed, "Master listening on ")
	if administrator < 0 || password < administrator || listening < 0 {
		t.Fatalf("first start did not print the administrator, its password and the listening line:\n%s", printed)
	}
	if listening < password {
		t.Fatalf("the listening line comes before the initial password:\n%s", printed)
	}
}
