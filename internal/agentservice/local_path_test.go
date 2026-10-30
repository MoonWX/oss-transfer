package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
)

func TestLocalPathObservesChangesWithoutCreatingDirectories(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target")
	service := &Service{}
	check := func(want string) {
		t.Helper()
		payload, _ := json.Marshal(PathRequest{Path: path})
		response, err := service.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: "local_path", Payload: payload}})
		var result struct {
			PathType string `json:"pathType"`
		}
		if err != nil || response.Status != 200 || json.Unmarshal(response.Payload, &result) != nil || result.PathType != want {
			t.Fatalf("local_path: want %s, got %+v, %v", want, response, err)
		}
	}
	check("MISSING")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight created the missing target", err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	check("DIRECTORY")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("business data"), 0600); err != nil {
		t.Fatal(err)
	}
	check("FILE")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check("MISSING")
}
