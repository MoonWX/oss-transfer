package softwareupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// versionArchive builds a valid release archive whose VERSION and checksum
// manifest describe version.
func versionArchive(t *testing.T, version string) []byte {
	t.Helper()
	target := platform()
	return archiveFixture(t, target, func(files map[string][]byte) {
		files["VERSION"] = []byte("Version: " + version + "\nFullCommit: " + testCommit + "\nPlatform: " + target + "\n")
		var sums strings.Builder
		for name, data := range files {
			if name != "SHA256SUMS" {
				fmt.Fprintf(&sums, "%s  %s\n", digest(data), name)
			}
		}
		files["SHA256SUMS"] = []byte(sums.String())
	}, "", false)
}

type fakeRelease struct {
	version           string // published version served by every metadata path
	prerelease, draft bool   // GitHub flags
	githubFirst       bool
	mu                sync.Mutex
	paths             []string
}

func (f *fakeRelease) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// fakeReleaseClient serves both the OSS release.json layout and the GitHub
// releases API for one published version.
func fakeReleaseClient(t *testing.T, f *fakeRelease) *client {
	t.Helper()
	archive := versionArchive(t, f.version)
	archiveName := "oss-transfer-" + strings.ReplaceAll(platform(), "/", "-") + ".tar.gz"
	if runtime.GOOS == "windows" {
		archiveName = strings.TrimSuffix(archiveName, ".tar.gz") + ".zip"
	}
	installer := []byte("# fixture native installer\n")
	tag := "v" + f.version
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/cdn-cgi/trace":
			if f.githubFirst {
				_, _ = io.WriteString(w, "loc=US\n")
			} else {
				_, _ = io.WriteString(w, "loc=CN\n")
			}
		case strings.HasSuffix(r.URL.Path, "/release.json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"format": "oss-transfer-release-v1", "version": f.version, "tag": tag, "commit": testCommit, "assets": []asset{{Name: archiveName, Size: int64(len(archive)), SHA256: digest(archive)}, {Name: "native-install.sh", Size: int64(len(installer)), SHA256: digest(installer)}, {Name: "install.sh", Size: int64(len(installer)), SHA256: digest(installer)}, {Name: "Install.ps1", Size: int64(len(installer)), SHA256: digest(installer)}}})
		case strings.HasPrefix(r.URL.Path, "/repos/MoonWX/oss-transfer/releases/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "draft": f.draft, "prerelease": f.prerelease, "assets": []map[string]any{{"name": archiveName, "size": len(archive), "digest": "sha256:" + digest(archive), "browser_download_url": releaseBase + "/download/" + tag + "/" + archiveName}}})
		case strings.HasSuffix(r.URL.Path, archiveName):
			_, _ = w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "install.sh"), strings.HasSuffix(r.URL.Path, "Install.ps1"):
			_, _ = w.Write(installer)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	return &client{http: &http.Client{Transport: redirectTransport{base, server.Client().Transport}, Timeout: 5 * time.Second}}
}

func TestReleaseMetadataRequiresExplicitPrereleaseOptIn(t *testing.T) {
	ctx := context.Background()
	for _, source := range []string{"jdoss", "github"} {
		t.Run(source, func(t *testing.T) {
			f := &fakeRelease{version: "1.0.1-rc.1", prerelease: true}
			c := fakeReleaseClient(t, f)
			if _, err := c.releaseFrom(ctx, source, "1.0.1-rc.1", false); !errors.Is(err, ErrPrereleaseNotAllowed) {
				t.Fatalf("prerelease metadata accepted without opt-in: %v", err)
			}
			release, err := c.releaseFrom(ctx, source, "1.0.1-rc.1", true)
			if err != nil || release.Version != "1.0.1-rc.1" || release.Tag != "v1.0.1-rc.1" || release.URL != releaseBase+"/tag/v1.0.1-rc.1" {
				t.Fatalf("%+v %v", release, err)
			}
			// "latest" never selects a prerelease, even with the flag.
			if _, err := c.releaseFrom(ctx, source, "latest", true); err == nil {
				t.Fatal("latest resolved to a prerelease")
			}
			// A different published version never satisfies an exact request.
			if _, err := c.releaseFrom(ctx, source, "1.0.1-rc.2", true); err == nil {
				t.Fatal("version mismatch accepted")
			}
			want := map[string]string{"jdoss": "/oss-transfer/v1.0.1-rc.1/release.json", "github": "/repos/MoonWX/oss-transfer/releases/tags/v1.0.1-rc.1"}[source]
			found := false
			for _, path := range f.requested() {
				found = found || path == want
			}
			if !found {
				t.Fatalf("versioned metadata %s was not used: %v", want, f.requested())
			}
		})
	}
	drafted := fakeReleaseClient(t, &fakeRelease{version: "1.0.1-rc.1", prerelease: true, draft: true})
	if _, err := drafted.releaseFrom(ctx, "github", "1.0.1-rc.1", true); err == nil {
		t.Fatal("draft release accepted")
	}
	// A stable request rejects metadata GitHub marks as prerelease.
	marked := fakeReleaseClient(t, &fakeRelease{version: "1.0.1", prerelease: true})
	if _, err := marked.releaseFrom(ctx, "github", "1.0.1", false); !errors.Is(err, ErrPrereleaseNotAllowed) {
		t.Fatalf("GitHub prerelease flag ignored: %v", err)
	}
}

func TestCheckOffersOnlyStableAndResolveNeedsFlag(t *testing.T) {
	ctx := context.Background()
	prerelease := &Manager{client: fakeReleaseClient(t, &fakeRelease{version: "1.0.1-rc.1", prerelease: true})}
	if release, err := prerelease.Check(ctx); err == nil {
		t.Fatalf("Check offered a prerelease: %+v", release)
	}
	stable := &fakeRelease{version: "1.0.1", githubFirst: true}
	manager := &Manager{client: fakeReleaseClient(t, stable)}
	if release, err := manager.Check(ctx); err != nil || release.Version != "1.0.1" {
		t.Fatal(release, err)
	}
	if paths := stable.requested(); !containsPath(paths, "/repos/MoonWX/oss-transfer/releases/latest") {
		t.Fatalf("Check did not use the latest stable endpoint: %v", paths)
	}
	explicit := &fakeRelease{version: "1.0.2-beta.1", prerelease: true}
	resolver := &Manager{client: fakeReleaseClient(t, explicit)}
	if _, err := resolver.Resolve(ctx, "1.0.2-beta.1", false); !errors.Is(err, ErrPrereleaseNotAllowed) {
		t.Fatal(err)
	}
	if len(explicit.requested()) != 0 {
		t.Fatal("rejected prerelease contacted the release source")
	}
	if release, err := resolver.Resolve(ctx, "v1.0.2-beta.1", true); err != nil || release.Version != "1.0.2-beta.1" {
		t.Fatal(release, err)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

// installedAt rewrites the managed fixture to report version as installed.
func installedAt(t *testing.T, version string) (Options, string) {
	t.Helper()
	options, executable := managedFixture(t)
	options.Version = version
	writeInstalled(t, options, executable, version, filepath.Join(filepath.Dir(options.DataDirectory), "oss-transfer-cli"+executableSuffix()))
	return options, executable
}

func TestStartRequiresFlagForPrereleaseAndOrdersBySemVer(t *testing.T) {
	for _, tc := range []struct {
		installed, target string
		allow             bool
		want              error
	}{
		{"1.0.0", "1.0.1-rc.1", false, ErrPrereleaseNotAllowed},
		{"1.0.0", "1.0.0-rc.1", true, ErrNotNewer},
		{"1.0.0", "1.0.1-rc.01", true, ErrInvalidVersion},
		{"1.0.1-beta.2", "1.0.1-beta.1", true, ErrNotNewer},
		{"1.0.1-beta.1", "1.0.1-beta.1", true, ErrNotNewer},
		{"1.0.1-beta.1", "1.0.1-beta.2", false, ErrPrereleaseNotAllowed},
		{"1.0.0", "1.0.1-rc.1", true, nil},
		{"1.0.1-beta.1", "1.0.1-beta.2", true, nil},
		{"1.0.1-beta.1", "1.0.1", false, nil}, // beta -> stable needs no flag
		{"1.0.1-rc.1", "1.0.1", true, nil},    // the flag permits, never requires
	} {
		t.Run(tc.installed+"->"+tc.target, func(t *testing.T) {
			options, executable := installedAt(t, tc.installed)
			var requestPath string
			manager := &Manager{options: options, executable: executable, info: Info{Supported: true}, launch: func(_, request, _ string) error {
				requestPath = request
				return nil
			}}
			status, err := manager.Start(context.Background(), tc.target, tc.allow)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
			if tc.want != nil {
				if requestPath != "" {
					t.Fatal("rejected version launched the helper")
				}
				return
			}
			var req request
			if err := readJSON(requestPath, &req); err != nil {
				t.Fatal(err)
			}
			if status.Version != tc.target || req.Version != tc.target || req.AllowPrerelease != IsPrerelease(tc.target) {
				t.Fatalf("status=%+v request=%+v", status, req)
			}
		})
	}
	if _, err := inspectInstallation(Options{Role: "master", Version: "4.1.0-dev"}, "/x"); err == nil || !strings.Contains(err.Error(), "release version") {
		t.Fatal("development build accepted as an updatable installation", err)
	}
}

func TestHelperInstallsPrereleaseOnlyWithPersistedFlag(t *testing.T) {
	for _, tc := range []struct {
		name, installed, target string
		allow, wantRun          bool
	}{
		{"prerelease with flag", "1.0.0", "1.0.1-rc.1", true, true},
		{"prerelease without flag", "1.0.0", "1.0.1-rc.1", false, false},
		{"beta to stable", "1.0.1-rc.1", "1.0.1", false, true},
		{"older prerelease", "1.0.1", "1.0.1-rc.1", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, executable := installedAt(t, tc.installed)
			req := request{ID: strings.Repeat("b", 32), Version: tc.target, AllowPrerelease: tc.allow, Options: options, Executable: executable}
			work := filepath.Join(options.DataDirectory, "runtime", "updates", req.ID)
			path := filepath.Join(work, "request.json")
			if err := writeJSON(path, req); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(filepath.Dir(work), "status.json"), Status{ID: req.ID, Version: req.Version, Phase: "QUEUED", UpdatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			f := &fakeRelease{version: tc.target, prerelease: IsPrerelease(tc.target)}
			ran := false
			err := runHelper(path, fakeReleaseClient(t, f), func(cmd *exec.Cmd) error {
				ran = true
				manifest, err := os.ReadFile(filepath.Join(filepath.Dir(path), "package-0", "manifest.tsv"))
				if err != nil || !strings.Contains(string(manifest), "version\t"+tc.target+"\n") {
					t.Fatalf("staged manifest: %q %v", manifest, err)
				}
				writeInstalled(t, options, executable, tc.target, filepath.Join(filepath.Dir(options.DataDirectory), "oss-transfer-cli"+executableSuffix()))
				return nil
			}, func() error { return nil })
			if ran != tc.wantRun || (err == nil) != tc.wantRun {
				t.Fatalf("ran=%v err=%v", ran, err)
			}
			if !tc.wantRun && len(f.requested()) != 0 {
				t.Fatal("invalid request contacted the release source")
			}
			if tc.wantRun {
				if status := (&Manager{options: options}).Status(); status.Phase != "COMPLETED" {
					t.Fatal(status)
				}
			}
		})
	}
}
