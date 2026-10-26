package master

import (
	"net/http/httptest"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

type headerOnlyUpdater struct{ fakeSoftwareUpdater }

func (*headerOnlyUpdater) Status() softwareupdate.Status {
	panic("API build header must not read update state from disk")
}

func TestAPIReportsRunningBuildWithoutReadingUpdateState(t *testing.T) {
	f := newAgentHTTPFixture(t)
	updater := &headerOnlyUpdater{}
	updater.info.Commit = "13246ee"
	f.master.options.Updater = updater
	for _, path := range []string{"/api/setup/status", "/api/not-found"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		f.master.ServeHTTP(w, r)
		if got := w.Header().Get("X-OSS-Transfer-Commit"); got != "13246ee" {
			t.Fatalf("%s build header = %q", path, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s cache control = %q", path, got)
		}
	}
	for _, commit := range []string{"", "unknown"} {
		updater.info.Commit = commit
		w := httptest.NewRecorder()
		f.master.ServeHTTP(w, httptest.NewRequest("GET", "/api/setup/status", nil))
		if got := w.Header().Get("X-OSS-Transfer-Commit"); got != "" {
			t.Fatalf("unknown build advertised as %q", got)
		}
	}
	f.master.options.Updater = nil
	w := httptest.NewRecorder()
	f.master.ServeHTTP(w, httptest.NewRequest("GET", "/api/setup/status", nil))
	if got := w.Header().Get("X-OSS-Transfer-Commit"); got != "" {
		t.Fatalf("unmanaged build advertised as %q", got)
	}
}
