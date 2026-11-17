package softwareupdate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type sourceReplies func(*http.Request) (*http.Response, error)

func (f sourceReplies) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A check in which no release source answers cannot succeed by trying again;
// one in which a source answered with an error is a different failure. Both
// keep the reason each source gave.
func TestSourcesUnreachableOnlyWhenNoSourceAnswered(t *testing.T) {
	unreachable := errors.New("dial tcp: lookup release.example: no such host")
	for _, test := range []struct {
		name  string
		reply sourceReplies
		want  bool
	}{
		{"no source answers", func(*http.Request) (*http.Response, error) { return nil, unreachable }, true},
		{"GitHub refuses, OSS unreachable", func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "api.github.com" {
				return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("rate limited")), Request: r}, nil
			}
			return nil, unreachable
		}, false},
	} {
		c := &client{http: &http.Client{Transport: test.reply}}
		_, err := c.release(context.Background(), "latest", false)
		if err == nil || SourcesUnreachable(err) != test.want {
			t.Fatalf("%s: unreachable=%v for %v", test.name, SourcesUnreachable(err), err)
		}
		for _, source := range []string{"github: ", "jdoss: "} {
			if !strings.Contains(err.Error(), source) {
				t.Fatalf("%s: the reason of %q is missing from %q", test.name, source, err)
			}
		}
	}
	if SourcesUnreachable(nil) || SourcesUnreachable(unreachable) {
		t.Fatal("an error that is not a release lookup is called unreachable")
	}
}
