package master

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

func packageDigest(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }
func packageTestManifest(targets []string, assets map[string][]byte) []byte {
	var text strings.Builder
	fmt.Fprintf(&text, "format\toss-transfer-go-v1\nversion\t4.1.21\ncommit\t%s\n", strings.Repeat("c", 40))
	for _, target := range targets {
		parts := strings.SplitN(target, "-", 2)
		suffix, ext := ".tar.gz", ""
		if parts[0] == "windows" {
			suffix, ext = ".zip", ".exe"
		}
		name := "oss-transfer-" + target + suffix
		body := assets[name]
		fmt.Fprintf(&text, "archive\t%s\t%s\t%s\t%s\t%d\n", parts[0], parts[1], name, packageDigest(body), len(body))
		for _, role := range []string{"master", "agent", "cli"} {
			fmt.Fprintf(&text, "file\t%s\t%s\t%s\toss-transfer-%s-%s%s\t%s\t1\n", parts[0], parts[1], role, role, target, ext, strings.Repeat("b", 64))
		}
	}
	for _, name := range []string{"install.sh", "native-install.sh", "Install.ps1"} {
		body := assets[name]
		fmt.Fprintf(&text, "asset\t%s\t%s\t%d\n", name, packageDigest(body), len(body))
	}
	return []byte(text.String())
}
func packageFixture(t *testing.T, f *agentHTTPFixture, targets ...string) (string, map[string][]byte) {
	t.Helper()
	f.master.options.DataDirectory = t.TempDir()
	f.master.options.AgentArtifactVersion = "v4.1.21"
	f.master.options.AgentArtifactBase = "https://invalid.example/releases"
	assets := map[string][]byte{"install.sh": []byte("#!/bin/bash\nprintf '%s\\n' \"$@\" > \"$PACKAGE_RESULT\"\ncat >> \"$PACKAGE_RESULT\"\n"), "native-install.sh": []byte("# native installer\n"), "Install.ps1": []byte("# Windows installer\n")}
	for _, target := range targets {
		suffix := ".tar.gz"
		if strings.HasPrefix(target, "windows-") {
			suffix = ".zip"
		}
		assets["oss-transfer-"+target+suffix] = []byte("verified fixture " + target)
	}
	dir := filepath.Join(f.master.options.DataDirectory, "packages")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, raw := range assets {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.tsv"), packageTestManifest(targets, assets), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, assets
}
func packageResponse(t *testing.T, address string) (int, []byte) {
	t.Helper()
	response, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw
}

func TestMasterAgentPackagesJoinUsesOnlyMasterAndPreservesUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash bootstrap")
	}
	f := newAgentHTTPFixture(t)
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	target := runtime.GOOS + "-" + arch
	_, _ = packageFixture(t, f, target)
	invite, err := f.master.agents.registry.InviteForMaster("offline-agent", "", f.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	link := f.http.URL + "/api/agents/bootstrap/" + invite.Token
	code, raw := packageResponse(t, link)
	if code != 200 || strings.Contains(string(raw), "invalid.example") || !strings.Contains(string(raw), "/files") {
		t.Fatal(code, string(raw))
	}
	result := filepath.Join(t.TempDir(), "invocation")
	command := exec.Command("bash", "-c", bootstrapCommand(link))
	command.Env = append(os.Environ(), "PACKAGE_RESULT="+result)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap failed: %v\n%s", err, output)
	}
	got, err := os.ReadFile(result)
	if err != nil || !strings.Contains(string(got), "--release-url\n"+link+"/files\n") || !strings.HasSuffix(string(got), invite.Token) {
		t.Fatal("same-Master installer invocation lost settings", err, string(got))
	}
	code, raw = packageResponse(t, link+"?source=upstream")
	if code != 200 || !strings.Contains(string(raw), "https://invalid.example/releases/v4.1.21/install.sh") {
		t.Fatal("original upstream scheme lost", code, string(raw))
	}
	code, raw = packageResponse(t, link+"?platform=windows")
	if code != 200 || !strings.Contains(string(raw), "/manifest.tsv?target=windows-") || !strings.Contains(string(raw), "-ReleaseUrl $release") {
		t.Fatal("Windows bootstrap missing same-Master packages", code, string(raw))
	}
}

func TestMasterAgentPackagesRequireLiveInvitationAndVerifiedRegularFiles(t *testing.T) {
	f := newAgentHTTPFixture(t)
	dir, assets := packageFixture(t, f, "linux-amd64")
	invite, err := f.master.agents.registry.Invite("package-access", "")
	if err != nil {
		t.Fatal(err)
	}
	link := f.http.URL + "/api/agents/bootstrap/" + invite.Token + "/files/"
	for name, expected := range assets {
		code, raw := packageResponse(t, link+name)
		if code != 200 || string(raw) != string(expected) {
			t.Fatalf("%s: %d %q", name, code, raw)
		}
	}
	for _, name := range []string{"master.db", "../master.db", "..%2fmaster.db", "install.sh.backup"} {
		code, _ := packageResponse(t, link+name)
		if code == 200 {
			t.Fatal("allowed non-package file", name)
		}
	}
	name := "oss-transfer-linux-amd64.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _ := packageResponse(t, link+name)
	if code != 503 {
		t.Fatal("served tampered package", code)
	}
	if err := os.Remove(filepath.Join(dir, "install.sh")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, assets["install.sh"], 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "install.sh")); err == nil {
		code, _ = packageResponse(t, link+"install.sh")
		if code != 503 {
			t.Fatal("served symlink package", code)
		}
	}
	credential, _ := cluster.Token()
	if _, err := f.master.agents.registry.Join(cluster.JoinRequest{Token: invite.Token, InstanceID: "installed", Credential: credential}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.tsv", "Install.ps1", name} {
		code, _ = packageResponse(t, link+name)
		if code != 404 {
			t.Fatal("used invitation still downloads", name, code)
		}
	}
}

func TestMasterAgentPackagesFetchMissingFileOnceAndRejectWrongChecksum(t *testing.T) {
	f := newAgentHTTPFixture(t)
	dir, assets := packageFixture(t, f, "linux-arm64")
	name := "oss-transfer-linux-arm64.tar.gz"
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var corrupt atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v4.1.21/"+name {
			http.NotFound(w, r)
			return
		}
		body := assets[name]
		if corrupt.Load() {
			body = []byte("wrong build")
		}
		w.Write(body)
	}))
	defer upstream.Close()
	f.master.options.AgentArtifactBase = upstream.URL
	invite, err := f.master.agents.registry.Invite("fetch", "")
	if err != nil {
		t.Fatal(err)
	}
	link := f.http.URL + "/api/agents/bootstrap/" + invite.Token + "/files/" + name
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, raw := packageResponse(t, link)
			if code != 200 || string(raw) != string(assets[name]) {
				t.Errorf("%d %s", code, raw)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate upstream downloads", calls.Load())
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	corrupt.Store(true)
	code, _ := packageResponse(t, link)
	if code != 503 {
		t.Fatal("wrong remote build accepted", code)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatal("invalid download was cached", err)
	}
}

func TestMasterAgentManifestAddsOnlyMatchingBuildAndCachedPlatformStaysOffline(t *testing.T) {
	for _, mode := range []string{"same build", "different commit", "changed overlap"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgentHTTPFixture(t)
			dir, assets := packageFixture(t, f, "linux-amd64")
			before, err := os.ReadFile(filepath.Join(dir, "manifest.tsv"))
			if err != nil {
				t.Fatal(err)
			}
			assets["oss-transfer-linux-arm64.tar.gz"] = []byte("arm64 build")
			complete := packageTestManifest([]string{"linux-amd64", "linux-arm64"}, assets)
			if mode == "different commit" {
				complete = []byte(strings.Replace(string(complete), strings.Repeat("c", 40), strings.Repeat("d", 40), 1))
			}
			if mode == "changed overlap" {
				complete = []byte(strings.Replace(string(complete), packageDigest(assets["install.sh"]), strings.Repeat("0", 64), 1))
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write(complete) }))
			defer upstream.Close()
			f.master.options.AgentArtifactBase = upstream.URL
			invite, err := f.master.agents.registry.Invite("cross-platform", "")
			if err != nil {
				t.Fatal(err)
			}
			link := f.http.URL + "/api/agents/bootstrap/" + invite.Token + "/files/manifest.tsv"
			code, _ := packageResponse(t, link+"?target=linux-amd64")
			if code != 200 || calls.Load() != 0 {
				t.Fatal("cached platform reached upstream", code, calls.Load())
			}
			code, raw := packageResponse(t, link+"?target=linux-arm64")
			after, err := os.ReadFile(filepath.Join(dir, "manifest.tsv"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "same build" {
				if code != 200 || string(raw) != string(complete) || string(after) != string(complete) {
					t.Fatal("same build platform unavailable", code, string(raw))
				}
			} else {
				if code != 503 || string(after) != string(before) {
					t.Fatal("unrelated build changed manifest", code)
				}
			}
		})
	}
}

func TestMasterAgentManifestRejectsAmbiguousOrUnsafeEntries(t *testing.T) {
	assets := map[string][]byte{"install.sh": []byte("a"), "native-install.sh": []byte("b"), "Install.ps1": []byte("c"), "oss-transfer-linux-amd64.tar.gz": []byte("archive")}
	valid := string(packageTestManifest([]string{"linux-amd64"}, assets))
	for _, raw := range []string{
		valid + "commit\t" + strings.Repeat("c", 40) + "\n",
		valid + "asset\tinstall.sh\t" + packageDigest(assets["install.sh"]) + "\t1\n",
		strings.Replace(valid, "asset\tinstall.sh\t", "asset\t../install.sh\t", 1),
		strings.Replace(valid, "archive\tlinux\tamd64\toss-transfer-linux-amd64.tar.gz", "archive\tlinux\tarm64\toss-transfer-linux-amd64.tar.gz", 1),
		strings.Replace(valid, "\t7\n", "\t1073741825\n", 1),
		strings.Replace(valid, "version\t4.1.21", "version\tlatest", 1),
	} {
		if _, err := parseAgentPackageManifest([]byte(raw)); err == nil {
			t.Fatal("accepted malformed package manifest", raw)
		}
	}
}

func TestMasterAgentPackagesMatchShortFullAndDirtyBuilds(t *testing.T) {
	for _, test := range []struct {
		running, packaged string
		valid             bool
	}{
		{strings.Repeat("c", 7), strings.Repeat("c", 40), true},
		{strings.Repeat("c", 40), strings.Repeat("c", 40), true},
		{strings.Repeat("c", 7) + "-dirty", strings.Repeat("c", 40) + "-dirty", true},
		{strings.Repeat("c", 40) + "-dirty", strings.Repeat("c", 40) + "-dirty", true},
		{strings.Repeat("d", 7), strings.Repeat("c", 40), false},
		{strings.Repeat("c", 7) + "-dirty", strings.Repeat("c", 40), false},
		{strings.Repeat("c", 7), strings.Repeat("c", 40) + "-dirty", false},
	} {
		t.Run(test.running+"-"+test.packaged, func(t *testing.T) {
			f := newAgentHTTPFixture(t)
			dir, _ := packageFixture(t, f, "linux-amd64")
			raw, err := os.ReadFile(filepath.Join(dir, "manifest.tsv"))
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.Replace(string(raw), "commit\t"+strings.Repeat("c", 40), "commit\t"+test.packaged, 1))
			if err = os.WriteFile(filepath.Join(dir, "manifest.tsv"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			f.master.options.Updater = &fakeSoftwareUpdater{info: softwareupdate.Info{Commit: test.running}}
			root, _, err := f.master.openAgentPackages()
			if root != nil {
				root.Close()
			}
			if (err == nil) != test.valid {
				t.Fatalf("build accepted=%v wanted=%v error=%v", err == nil, test.valid, err)
			}
		})
	}
}

func TestMasterAgentCachedPackageDoesNotWaitForSlowPlatformDownload(t *testing.T) {
	f := newAgentHTTPFixture(t)
	dir, assets := packageFixture(t, f, "linux-amd64")
	archive := "oss-transfer-linux-amd64.tar.gz"
	if err := os.Remove(filepath.Join(dir, archive)); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			w.Write(assets[archive])
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	// Ensure the upstream handler is released before httptest waits for it.
	defer unblock()
	f.master.options.AgentArtifactBase = upstream.URL
	invite, err := f.master.agents.registry.Invite("slow-platform", "")
	if err != nil {
		t.Fatal(err)
	}
	base := f.http.URL + "/api/agents/bootstrap/" + invite.Token + "/files/"
	completed := make(chan error, 1)
	go func() {
		response, err := http.Get(base + archive)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("archive returned %d", response.StatusCode)
			}
		}
		completed <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream download did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"install.sh", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("cached installer waited behind the upstream download", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != string(assets["install.sh"]) {
		t.Fatal("cached installer was unavailable", response.StatusCode, err)
	}
	unblock()
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}
