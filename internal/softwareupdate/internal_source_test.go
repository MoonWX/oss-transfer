package softwareupdate

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// onInternalNetwork returns the client of a host that reaches JD Cloud's
// internal endpoint, and the log of the requests it makes.
func onInternalNetwork(t *testing.T, f *fakeRelease, reachable bool) (*client, *requestLog) {
	t.Helper()
	c := fakeReleaseClient(t, f)
	transport := c.http.Transport.(redirectTransport)
	transport.internal, transport.log = reachable, &requestLog{}
	c.http.Transport = transport
	return c, transport.log
}

func TestReachableInternalEndpointIsUsedForMetadataAndEveryDownload(t *testing.T) {
	for _, githubFirst := range []bool{false, true} {
		f := &fakeRelease{version: "1.0.1", githubFirst: githubFirst}
		c, log := onInternalNetwork(t, f, true)
		ctx := context.Background()
		if order := c.sourceOrder(ctx); !reflect.DeepEqual(order, []string{"jdoss-internal", "jdoss", "github"}) {
			t.Fatalf("source order on the internal network: %v", order)
		}
		release, err := c.release(ctx, "latest", false)
		if err != nil || release.source != "jdoss-internal" || release.Version != "1.0.1" {
			t.Fatalf("latest release from the internal endpoint: %+v %v", release, err)
		}
		for _, a := range release.assets {
			if a.URL != internalBase+"/v1.0.1/"+a.Name {
				t.Fatalf("asset %s does not download through the internal endpoint: %s", a.Name, a.URL)
			}
		}
		stage, commit, err := c.prepare(ctx, t.TempDir(), "1.0.1", false, platform())
		if err != nil || commit != testCommit {
			t.Fatalf("prepare through the internal endpoint: %q %v", commit, err)
		}
		for _, name := range []string{"manifest.tsv", "native-install.sh", "install.sh", "Install.ps1"} {
			if info, err := os.Stat(filepath.Join(stage, name)); err != nil || info.Size() == 0 {
				t.Fatalf("staged %s: %v", name, err)
			}
		}
		// A host without a public route must not need one: no location
		// lookup, no GitHub and no public mirror.
		hosts, paths, _ := log.snapshot()
		for i, host := range hosts {
			if host != internalHost {
				t.Fatalf("request %d left the internal endpoint: %s%s", i, host, paths[i])
			}
		}
		if containsPath(f.requested(), "/cdn-cgi/trace") {
			t.Fatal("the public IP location was looked up on the internal network")
		}
	}
}

func TestUnreachableInternalEndpointCostsOneShortAttempt(t *testing.T) {
	for githubFirst, want := range map[bool][]string{false: {"jdoss", "github"}, true: {"github", "jdoss"}} {
		f := &fakeRelease{version: "1.0.1", githubFirst: githubFirst}
		c, log := onInternalNetwork(t, f, false)
		if order := c.sourceOrder(context.Background()); !reflect.DeepEqual(order, want) {
			t.Fatalf("public source order (GitHub first %v): %v", githubFirst, order)
		}
		hosts, paths, deadlines := log.snapshot()
		if len(hosts) != 2 || hosts[0] != internalHost || paths[0] != "/oss-transfer/latest/release.json" || hosts[1] != "www.cloudflare.com" {
			t.Fatalf("requests of one source selection: %v %v", hosts, paths)
		}
		// The attempt is bounded by its own deadline, not by the caller's.
		if deadlines[0] <= 0 || deadlines[0] > 2*time.Second {
			t.Fatalf("internal attempt may wait %v", deadlines[0])
		}
		release, err := c.release(context.Background(), "latest", false)
		if err != nil || release.source != want[0] || strings.Contains(release.assets[0].URL, internalHost) {
			t.Fatalf("release from the public sources: %+v %v", release, err)
		}
	}
}

func TestInternalEndpointWithoutTheRequestedVersionFallsBackToTheMirror(t *testing.T) {
	f := &fakeRelease{version: "1.0.1", internalMissing: true}
	c, log := onInternalNetwork(t, f, true)
	ctx := context.Background()
	release, err := c.release(ctx, "1.0.1", false)
	if err != nil || release.source != "jdoss" {
		t.Fatalf("release after the internal endpoint lacked the version: %+v %v", release, err)
	}
	for _, a := range release.assets {
		if a.URL != ossBase+"/v1.0.1/"+a.Name {
			t.Fatalf("asset %s kept the internal address: %s", a.Name, a.URL)
		}
	}
	stage, _, err := c.prepare(ctx, t.TempDir(), "1.0.1", false, platform())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(stage) != "package-1" {
		t.Fatalf("the mirror is the second source, staged in %s", stage)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(stage), "package-0")); !os.IsNotExist(err) {
		t.Fatalf("the failed internal stage was kept: %v", err)
	}
	hosts, _, _ := log.snapshot()
	for _, host := range hosts {
		if host != internalHost && host != "moonwx.s3.cn-north-1.jdcloud-oss.com" {
			t.Fatalf("fallback went beyond the public mirror: %s", host)
		}
	}
}

func TestInternalSourceIsAnOSSSource(t *testing.T) {
	for source, want := range map[string]string{"jdoss": ossBase, "jdoss-internal": internalBase} {
		if base, ok := ossSourceBase(source); !ok || base != want {
			t.Fatalf("%s: %q %v", source, base, ok)
		}
	}
	if _, ok := ossSourceBase("github"); ok {
		t.Fatal("GitHub is not an OSS source")
	}
	if !strings.HasPrefix(internalBase, "https://"+internalHost+"/") {
		t.Fatalf("fixture host differs from %s", internalBase)
	}
	var _ http.RoundTripper = redirectTransport{}
}
