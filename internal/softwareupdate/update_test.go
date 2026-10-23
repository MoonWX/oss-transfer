package softwareupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/MoonWX/oss-transfer/internal/servicehost"
	bolt "go.etcd.io/bbolt"
)

const testCommit = "1234567890abcdef1234567890abcdef12345678"

func digest(raw []byte) string { value := sha256.Sum256(raw); return hex.EncodeToString(value[:]) }

func archiveFixture(t *testing.T, target string, mutate func(map[string][]byte), extra string, link bool) []byte {
	t.Helper()
	ext := ""
	if strings.HasPrefix(target, "windows/") {
		ext = ".exe"
	}
	files := map[string][]byte{"VERSION": []byte("Version: 1.0.1\nFullCommit: " + testCommit + "\nPlatform: " + target + "\n")}
	for _, role := range []string{"master", "agent", "cli"} {
		files["oss-transfer-"+role+ext] = []byte("binary fixture " + role)
	}
	var sums strings.Builder
	for name, data := range files {
		fmt.Fprintf(&sums, "%s  %s\n", digest(data), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	if mutate != nil {
		mutate(files)
	}
	root := "oss-transfer-" + strings.ReplaceAll(target, "/", "-") + "/"
	var raw bytes.Buffer
	if ext != "" {
		writer := zip.NewWriter(&raw)
		for name, data := range files {
			member, err := writer.Create(root + name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = member.Write(data)
		}
		if extra != "" {
			header := &zip.FileHeader{Name: extra}
			if link {
				header.SetMode(os.ModeSymlink | 0777)
			}
			member, err := writer.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = member.Write([]byte("bad"))
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed := gzip.NewWriter(&raw)
		writer := tar.NewWriter(compressed)
		for name, data := range files {
			if err := writer.WriteHeader(&tar.Header{Name: root + name, Mode: 0755, Size: int64(len(data))}); err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write(data)
		}
		if extra != "" {
			header := &tar.Header{Name: extra, Mode: 0755, Size: 3}
			if link {
				header.Typeflag = tar.TypeSymlink
				header.Linkname = "/tmp/other"
				header.Size = 0
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if !link {
				_, _ = writer.Write([]byte("bad"))
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return raw.Bytes()
}

func TestVerifiedArchivesRejectTamperingAndUnsafeMembers(t *testing.T) {
	for _, target := range []string{"linux/amd64", "windows/arm64"} {
		for _, test := range []struct {
			name            string
			mutate          func(map[string][]byte)
			extra           string
			link, wantError bool
		}{
			{name: "valid"},
			{name: "changed executable", mutate: func(files map[string][]byte) {
				for name := range files {
					if strings.HasPrefix(name, "oss-transfer-agent") {
						files[name] = []byte("tampered")
					}
				}
			}, wantError: true},
			{name: "wrong version", mutate: func(files map[string][]byte) { files["VERSION"] = []byte("Version: 2.0.0\n") }, wantError: true},
			{name: "traversal", extra: "../outside", wantError: true},
			{name: "symlink", extra: "oss-transfer-" + strings.ReplaceAll(target, "/", "-") + "/README.md", link: true, wantError: true},
			{name: "duplicate", extra: "oss-transfer-" + strings.ReplaceAll(target, "/", "-") + "/VERSION", wantError: true},
		} {
			t.Run(target+"/"+test.name, func(t *testing.T) {
				ext := ".tar.gz"
				if strings.HasPrefix(target, "windows/") {
					ext = ".zip"
				}
				path := filepath.Join(t.TempDir(), "archive"+ext)
				if err := os.WriteFile(path, archiveFixture(t, target, test.mutate, test.extra, test.link), 0600); err != nil {
					t.Fatal(err)
				}
				identity, err := validateArchive(path, "1.0.1", target)
				if (err != nil) != test.wantError {
					t.Fatalf("error=%v wantError=%v", err, test.wantError)
				}
				if err == nil && identity.commit != testCommit {
					t.Fatal(identity)
				}
			})
		}
	}
}

type redirectTransport struct {
	base      *url.URL
	transport http.RoundTripper
}

func (r redirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	address := *request.URL
	copy.URL = &address
	copy.URL.Scheme = r.base.Scheme
	copy.URL.Host = r.base.Host
	return r.transport.RoundTrip(copy)
}
func releaseServer(t *testing.T, archive []byte, tamper bool) (*client, *[]string) {
	t.Helper()
	paths := []string{}
	var mu sync.Mutex
	archiveName := "oss-transfer-" + strings.ReplaceAll(platform(), "/", "-") + ".tar.gz"
	installerName := "native-install.sh"
	if runtime.GOOS == "windows" {
		archiveName = strings.TrimSuffix(archiveName, ".tar.gz") + ".zip"
		installerName = "Install.ps1"
	}
	installer := []byte("# fixture native installer\n")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/cdn-cgi/trace":
			_, _ = io.WriteString(w, "loc=CN\n")
		case strings.HasSuffix(r.URL.Path, "release.json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"format": "oss-transfer-release-v1", "version": "1.0.1", "tag": "v1.0.1", "commit": testCommit, "assets": []asset{{Name: archiveName, Size: int64(len(archive)), SHA256: digest(archive)}, {Name: installerName, Size: int64(len(installer)), SHA256: digest(installer)}}})
		case strings.HasSuffix(r.URL.Path, archiveName):
			if tamper {
				archive = bytes.Clone(archive)
				archive[len(archive)-1] ^= 1
			}
			_, _ = w.Write(archive)
		case strings.HasSuffix(r.URL.Path, installerName):
			_, _ = w.Write(installer)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	return &client{http: &http.Client{Transport: redirectTransport{base, server.Client().Transport}, Timeout: time.Second}}, &paths
}

func managedFixture(t *testing.T) (Options, string) {
	t.Helper()
	root := t.TempDir()
	install := filepath.Join(root, "bin")
	data := filepath.Join(root, "data")
	configPath := filepath.Join(root, "config", "service.json")
	for _, directory := range []string{install, data, filepath.Dir(configPath)} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(install, "oss-transfer-master"+executableSuffix())
	cli := filepath.Join(root, "oss-transfer-cli"+executableSuffix())
	for _, path := range []string{executable, cli} {
		if err := os.WriteFile(path, []byte("old binary"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	options := Options{Role: "master", Version: "1.0.0", Commit: testCommit, DataDirectory: data, ConfigPath: configPath, ServiceName: "oss-transfer-test"}
	config := servicehost.Config{ServiceName: options.ServiceName, DataDirectory: data, Listen: "127.0.0.1:1685", ReadyFile: filepath.Join(data, "runtime", "ready"), LogFile: filepath.Join(data, "logs", "service.log")}
	if err := writeJSON(configPath, config); err != nil {
		t.Fatal(err)
	}
	writeInstalled(t, options, executable, "1.0.0", cli)
	return options, executable
}
func writeInstalled(t *testing.T, options Options, executable, version, cli string) {
	t.Helper()
	installed := installation{Role: options.Role, ServiceName: options.ServiceName, Config: options.ConfigPath, DataDir: options.DataDirectory, CLIPath: cli, Version: version, Unit: "/Library/LaunchDaemons/" + options.ServiceName + ".plist"}
	if runtime.GOOS == "darwin" {
		installed.Unit = filepath.Join(filepath.Dir(options.DataDirectory), "Library", "LaunchAgents", options.ServiceName+".plist")
	}
	if runtime.GOOS == "windows" {
		if err := writeJSON(filepath.Join(filepath.Dir(executable), "install.json"), installed); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw := fmt.Sprintf("role\t%s\nserviceName\t%s\nconfig\t%s\ndataDir\t%s\ncliPath\t%s\nversion\t%s\nunit\t%s\n", installed.Role, installed.ServiceName, installed.Config, installed.DataDir, installed.CLIPath, installed.Version, installed.Unit)
	if err := os.WriteFile(filepath.Join(filepath.Dir(executable), "install.tsv"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestStartRejectsDowngradesAndIsConcurrentIdempotent(t *testing.T) {
	options, executable := managedFixture(t)
	launches := 0
	manager := &Manager{options: options, executable: executable, info: Info{Supported: true}, launch: func(helper, request, log string) error {
		launches++
		if err := regular(helper); err != nil {
			return err
		}
		return regular(request)
	}}
	for _, version := range []string{"1.0.0", "0.9.9", "1.0.1-rc.1", "https://other.test/install.sh", "2.01.0"} {
		if _, err := manager.Start(context.Background(), version); err == nil {
			t.Fatalf("accepted %q", version)
		}
	}
	var wg sync.WaitGroup
	results := make(chan Status, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := manager.Start(context.Background(), "1.0.1")
			results <- status
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for result := range results {
		if id == "" {
			id = result.ID
		}
		if result.ID != id || result.Phase != "QUEUED" {
			t.Fatal(result)
		}
	}
	if launches != 1 {
		t.Fatalf("launched %d helpers", launches)
	}
	if _, err := manager.Start(context.Background(), "1.0.2"); err == nil {
		t.Fatal("accepted a competing update")
	}
	// A new service process may already report the target version while the
	// independent installer is still doing its readiness check.
	restarted := &Manager{options: options, executable: executable, info: Info{Supported: true}}
	restarted.options.Version = "1.0.1"
	if status, err := restarted.Start(context.Background(), "1.0.1"); err != nil || status.ID != id {
		t.Fatalf("restart lost active update idempotency: %+v %v", status, err)
	}
}

func TestDownloadPolicyRejectsInsecureRedirectAndUnstableRelease(t *testing.T) {
	client := newClient()
	url, _ := url.Parse("http://example.invalid/asset")
	if err := client.http.CheckRedirect(&http.Request{URL: url}, nil); err == nil {
		t.Fatal("accepted an HTTP downgrade")
	}
	for _, version := range []string{"1.0.1-rc.1", "v1.0.1/../../other", "https://other.invalid/archive"} {
		if _, err := client.release(context.Background(), version); err == nil {
			t.Fatalf("accepted %q", version)
		}
	}
}

func helperFixture(t *testing.T) (request, string) {
	t.Helper()
	options, executable := managedFixture(t)
	req := request{ID: strings.Repeat("a", 32), Version: "1.0.1", Options: options, Executable: executable}
	work := filepath.Join(options.DataDirectory, "runtime", "updates", req.ID)
	path := filepath.Join(work, "request.json")
	if err := writeJSON(path, req); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(filepath.Dir(work), "status.json"), Status{ID: req.ID, Version: req.Version, Phase: "QUEUED", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return req, path
}

func TestHelperStagesBeforeInstallerAndNeverReportsFailedUpgradeAsSuccess(t *testing.T) {
	for _, mode := range []string{"tampered", "installer failure", "exit zero without upgrade", "success", "shared CLI locked"} {
		t.Run(mode, func(t *testing.T) {
			req, path := helperFixture(t)
			archive := archiveFixture(t, platform(), nil, "", false)
			client, _ := releaseServer(t, archive, mode == "tampered")
			installed, err := inspectInstallation(req.Options, req.Executable)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "shared CLI locked" {
				lock, err := bolt.Open(installed.CLIPath+".update.lock", 0600, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			executed := false
			err = runHelper(path, client, func(cmd *exec.Cmd) error {
				executed = true
				joined := strings.Join(cmd.Args, " ")
				for _, value := range []string{"master", filepath.Dir(req.Executable), "package-0"} {
					if !strings.Contains(joined, value) {
						t.Fatalf("installer arguments lost managed identity: %v", cmd.Args)
					}
				}
				stage := filepath.Join(filepath.Dir(path), "package-0")
				manifest, err := os.ReadFile(filepath.Join(stage, "manifest.tsv"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(manifest), "commit\t"+testCommit) || !strings.Contains(string(manifest), "version\t1.0.1") {
					t.Fatal(string(manifest))
				}
				if mode == "installer failure" {
					_, _ = io.WriteString(cmd.Stderr, "Fixture rollback completed\n")
					return errors.New("installer exit 1")
				}
				if mode == "success" {
					writeInstalled(t, req.Options, req.Executable, "1.0.1", installed.CLIPath)
				}
				return nil
			}, func() error { return nil })
			manager := &Manager{options: req.Options}
			status := manager.Status()
			if mode == "success" {
				if err != nil || status.Phase != "COMPLETED" {
					t.Fatalf("%v %+v", err, status)
				}
			} else {
				if err == nil || status.Phase != "FAILED" {
					t.Fatalf("%v %+v", err, status)
				}
			}
			if (mode == "tampered" || mode == "shared CLI locked") && executed {
				t.Fatal("installer ran before download integrity and shared lock passed")
			}
			if mode == "installer failure" && !strings.Contains(status.Log, "Fixture rollback completed") {
				t.Fatal(status)
			}
			old, _ := os.ReadFile(req.Executable)
			if string(old) != "old binary" {
				t.Fatal("failed preparation changed the existing binary")
			}
		})
	}
}

func TestManagedIdentityAndVersionCheck(t *testing.T) {
	options, executable := managedFixture(t)
	if _, err := inspectInstallation(options, executable); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*Options){func(o *Options) { o.Role = "agent" }, func(o *Options) { o.ServiceName = "other" }, func(o *Options) { o.DataDirectory = filepath.Dir(o.DataDirectory) }, func(o *Options) { o.Version = "1.0.1" }} {
		changed := options
		edit(&changed)
		if _, err := inspectInstallation(changed, executable); err == nil {
			t.Fatal("accepted mismatched installed identity")
		}
	}
	client, paths := releaseServer(t, archiveFixture(t, platform(), nil, "", false), false)
	manager := &Manager{client: client}
	first, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != "1.0.1" || second.URL != releaseBase+"/tag/v1.0.1" {
		t.Fatal(first, second)
	}
	if len(*paths) != 2 {
		t.Fatalf("version checks were not cached: %v", *paths)
	}
}
