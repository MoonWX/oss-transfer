package softwareupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ossBase            = "https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer"
	githubAPI          = "https://api.github.com/repos/MoonWX/oss-transfer/releases"
	releaseBase        = "https://github.com/MoonWX/oss-transfer/releases"
	archiveLimit int64 = 1 << 30
)

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Controller interface {
	Info() Info
	Status() Status
	Start(context.Context, string) (Status, error)
	Check(context.Context) (Release, error)
}
type Release struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Commit  string `json:"commit,omitempty"`
	URL     string `json:"url"`
	assets  []asset
	source  string
}
type asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"-"`
}
type client struct{ http *http.Client }

func newClient() *client {
	return &client{http: &http.Client{
		Timeout: 12 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || req.URL.User != nil || len(via) >= 5 {
				return errors.New("Unsafe update download redirect")
			}
			return nil
		},
	}}
}
func (c *client) fetch(ctx context.Context, address string, limit int64, out io.Writer) (int64, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return 0, errors.New("Update downloads must use HTTPS")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "OSS-Transfer-Updater")
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Update download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return 0, errors.New("Update download exceeds the size limit")
	}
	n, err := io.Copy(out, io.LimitReader(response.Body, limit+1))
	if n > limit {
		return n, errors.New("Update download exceeds the size limit")
	}
	return n, err
}
func (c *client) metadata(ctx context.Context, address string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var body strings.Builder
	if _, err := c.fetch(ctx, address, 2<<20, &body); err != nil {
		return err
	}
	return json.Unmarshal([]byte(body.String()), out)
}
func (c *client) sourceOrder(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var body strings.Builder
	if _, err := c.fetch(ctx, "https://www.cloudflare.com/cdn-cgi/trace", 4096, &body); err == nil && strings.Contains("\n"+body.String(), "\nloc=CN\n") {
		return []string{"jdoss", "github"}
	}
	return []string{"github", "jdoss"}
}
func (c *client) release(ctx context.Context, version string) (Release, error) {
	if version != "latest" && !stableVersion.MatchString(version) {
		return Release{}, errors.New("Invalid release version")
	}
	var errs []error
	for _, source := range c.sourceOrder(ctx) {
		release, err := c.releaseFrom(ctx, source, version)
		if err == nil {
			return release, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", source, err))
	}
	return Release{}, errors.Join(errs...)
}
func (c *client) releaseFrom(ctx context.Context, source, version string) (Release, error) {
	var release Release
	if source == "jdoss" {
		path := "latest"
		if version != "latest" {
			path = "v" + version
		}
		var document struct {
			Format  string  `json:"format"`
			Version string  `json:"version"`
			Tag     string  `json:"tag"`
			Commit  string  `json:"commit"`
			Assets  []asset `json:"assets"`
		}
		if err := c.metadata(ctx, ossBase+"/"+path+"/release.json", &document); err != nil {
			return release, err
		}
		if document.Format != "oss-transfer-release-v1" || !commitPattern.MatchString(document.Commit) {
			return release, errors.New("Invalid OSS release identity")
		}
		release = Release{Version: document.Version, Tag: document.Tag, Commit: document.Commit, assets: document.Assets, source: source}
		for i := range release.assets {
			release.assets[i].URL = ossBase + "/" + release.Tag + "/" + release.assets[i].Name
		}
	} else if source == "github" {
		path := "/latest"
		if version != "latest" {
			path = "/tags/v" + version
		}
		var document struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
			Assets     []struct {
				Name   string `json:"name"`
				Size   int64  `json:"size"`
				Digest string `json:"digest"`
				URL    string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if err := c.metadata(ctx, githubAPI+path, &document); err != nil {
			return release, err
		}
		if document.Draft || document.Prerelease {
			return release, errors.New("Only published stable releases can be installed")
		}
		release = Release{Version: strings.TrimPrefix(document.Tag, "v"), Tag: document.Tag, source: source}
		for _, a := range document.Assets {
			if !strings.HasSuffix(a.Name, ".tar.gz") && !strings.HasSuffix(a.Name, ".zip") {
				continue
			}
			if a.URL != releaseBase+"/download/"+document.Tag+"/"+a.Name || !strings.HasPrefix(a.Digest, "sha256:") {
				return release, errors.New("GitHub archive is missing a trusted download checksum")
			}
			release.assets = append(release.assets, asset{Name: a.Name, Size: a.Size, SHA256: strings.TrimPrefix(a.Digest, "sha256:"), URL: a.URL})
		}
	} else {
		return release, errors.New("Unknown release source")
	}
	if !stableVersion.MatchString(release.Version) || release.Tag != "v"+release.Version || (version != "latest" && version != release.Version) {
		return Release{}, errors.New("Release version does not match the requested stable version")
	}
	seen := map[string]bool{}
	for _, a := range release.assets {
		if seen[a.Name] || a.Name == "." || filepath.Base(a.Name) != a.Name || strings.ContainsAny(a.Name, "/\\\x00\r\n\t") || !shaPattern.MatchString(a.SHA256) || a.Size < 1 || a.Size > archiveLimit {
			return Release{}, errors.New("Invalid release asset metadata")
		}
		seen[a.Name] = true
	}
	if len(release.assets) == 0 {
		return Release{}, errors.New("Release contains no downloadable archives")
	}
	release.URL = releaseBase + "/tag/" + release.Tag
	return release, nil
}
func (r Release) asset(name string) (asset, error) {
	for _, a := range r.assets {
		if a.Name == name {
			return a, nil
		}
	}
	return asset{}, fmt.Errorf("Release does not contain %s", name)
}
func (c *client) download(ctx context.Context, a asset, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, fetchErr := c.fetch(ctx, a.URL, a.Size, io.MultiWriter(file, hash))
	if fetchErr == nil {
		fetchErr = file.Sync()
	}
	err = errors.Join(fetchErr, file.Close())
	if err != nil {
		return err
	}
	if n != a.Size || hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("Checksum or size mismatch for %s", a.Name)
	}
	return nil
}
