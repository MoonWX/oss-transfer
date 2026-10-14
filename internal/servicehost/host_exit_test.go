package servicehost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceFailureSurvivesConsoleRestoration(t *testing.T) {
	for _, failure := range []error{errors.New("cannot bind listener: occupied"), context.Canceled, nil} {
		path := filepath.Join(t.TempDir(), "logs", "service.log")
		stdout, stderr := os.Stdout, os.Stderr
		err := Run(Options{Name: "qa-service", Role: "master", Version: "test", LogFile: path}, func(context.Context, func() error) error {
			return failure
		})
		if !errors.Is(err, failure) || os.Stdout != stdout || os.Stderr != stderr {
			t.Fatalf("service exit lost failure or retained log handles: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		wantError := failure != nil && !errors.Is(failure, context.Canceled)
		if strings.Contains(string(raw), "Service failed:") != wantError || wantError && !strings.Contains(string(raw), failure.Error()) {
			t.Fatalf("service log lost error or reported normal shutdown as failure: %q", raw)
		}
	}
}
