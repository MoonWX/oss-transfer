package master

import (
	"net/http"
	"testing"
	"time"
)

func TestServerTimeZoneNamesTheZoneOfTaskTimeFilters(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		zone *time.Location
		want string
	}{
		{time.UTC, "UTC"},
		{time.FixedZone("CST", 8*3600), "CST (UTC+08:00)"},
		{time.FixedZone("", -(5*3600 + 30*60)), "UTC-05:30"},
		{time.FixedZone("+0545", 5*3600+45*60), "UTC+05:45"},
	} {
		if got := serverTimeZone(at.In(c.zone)); got != c.want {
			t.Errorf("serverTimeZone(%v) = %q, want %q", c.zone, got, c.want)
		}
	}
}

func TestNodeInfoReportsTheServerTimeZone(t *testing.T) {
	f := newAgentHTTPFixture(t)
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/worker/node-info", f.admin, nil)
	if want := serverTimeZone(f.master.options.Now().In(time.Local)); status != http.StatusOK || body["timeZone"] != want {
		t.Fatal("node info lacks the server time zone", status, body, want)
	}
}
