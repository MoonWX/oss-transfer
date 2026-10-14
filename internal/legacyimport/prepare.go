package legacyimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// DirectNodeApproval authorizes a fresh Agent identity for a V4 direct worker
// (including execution on the old Master). Existing Agents retain their identity.
type DirectNodeApproval struct {
	Address string `json:"address"`
	Label   string `json:"label"`
}

type PrepareOptions struct {
	SnapshotPaths []string
	Destination   string
	PublicURL     string
	DirectNodes   map[string]DirectNodeApproval
}

type SourceManifest struct {
	Node   string `json:"node"`
	Role   string `json:"role"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Footer Footer `json:"integrity"`
}

type NodeIdentity struct {
	LegacyNode   string `json:"legacyNode"`
	InstanceID   string `json:"instanceId"`
	EnrollmentID string `json:"enrollmentId"`
	Address      string `json:"address"`
	Label        string `json:"label"`
	Revoked      bool   `json:"revoked"`
	Generated    bool   `json:"generated"`
	Database     string `json:"database"`
}

type PreparationManifest struct {
	SchemaVersion int              `json:"schemaVersion"`
	MigrationID   string           `json:"migrationId"`
	CreatedAt     time.Time        `json:"createdAt"`
	Pending       bool             `json:"pending"`
	Destination   string           `json:"destination"`
	PublicURL     string           `json:"publicUrl"`
	Sources       []SourceManifest `json:"sources"`
	Nodes         []NodeIdentity   `json:"nodes"`
	Accounts      int              `json:"accounts"`
	Connections   int              `json:"connections"`
}

// Prepared is a private, non-runnable workspace for the subsequent task/history
// converter. No production data or old snapshot is modified. Close releases DB
// handles but retains staging and raw snapshots for inspection/recovery.
type Prepared struct {
	Directory string
	Master    *state.Store
	Agents    map[string]*state.Store // keyed by old node identity, not IP guesswork
	Manifest  PreparationManifest
	plans     map[string]Plan
	paths     map[string]string
	closed    bool
}

func cleanOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(value, "\x00\r\n\t ") {
		return "", errors.New("migration public URL must be an HTTP or HTTPS origin")
	}
	return strings.TrimRight(value, "/"), nil
}

// Prepare requires a new destination. Refusing even an empty existing directory
// keeps publication from replacing anything another process may have created.
func Prepare(ctx context.Context, options PrepareOptions) (_ *Prepared, resultErr error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("offline migration preparation currently requires a POSIX filesystem")
	}
	if len(options.SnapshotPaths) == 0 || len(options.SnapshotPaths) > 10000 {
		return nil, errors.New("migration requires 1–10000 node snapshots")
	}
	publicURL, err := cleanOrigin(options.PublicURL)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(options.Destination) || filepath.Clean(options.Destination) != options.Destination {
		return nil, errors.New("migration destination must be a clean absolute path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(options.Destination))
	if err != nil {
		return nil, errors.New("migration destination parent must already exist")
	}
	destination := filepath.Join(parent, filepath.Base(options.Destination))
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("migration destination already exists or cannot be inspected")
	}
	staging, err := os.MkdirTemp(parent, ".oss-transfer-migration-")
	if err != nil {
		return nil, err
	}
	p := &Prepared{Directory: staging, Agents: map[string]*state.Store{}, plans: map[string]Plan{}, paths: map[string]string{}}
	defer func() {
		if resultErr != nil {
			// This directory is allocated by this call and never contains user data.
			resultErr = errors.Join(resultErr, p.Close(), os.RemoveAll(staging))
		}
	}()
	if err := os.Mkdir(filepath.Join(staging, "raw"), 0700); err != nil {
		return nil, err
	}
	p.Manifest = PreparationManifest{SchemaVersion: 1, MigrationID: filepath.Base(staging), CreatedAt: time.Now().UTC(), Pending: true, Destination: destination, PublicURL: publicURL, Sources: []SourceManifest{}, Nodes: []NodeIdentity{}}
	var plans []Plan
	for index, source := range options.SnapshotPaths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		relative := fmt.Sprintf("raw/%05d.jsonl", index+1)
		copied := filepath.Join(staging, filepath.FromSlash(relative))
		sum, err := copySnapshot(ctx, source, copied)
		if err != nil {
			return nil, fmt.Errorf("copy node snapshot %d: %w", index+1, err)
		}
		f, err := os.Open(copied)
		if err != nil {
			return nil, err
		}
		plan, readErr := Inspect(ctx, f)
		closeErr := f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if _, exists := p.plans[plan.Header.Node]; exists {
			return nil, invalid("duplicate node snapshot")
		}
		p.plans[plan.Header.Node], p.paths[plan.Header.Node] = plan, copied
		plans = append(plans, plan)
		p.Manifest.Sources = append(p.Manifest.Sources, SourceManifest{Node: plan.Header.Node, Role: plan.Header.Role, Path: relative, SHA256: sum, Footer: plan.Integrity})
	}
	cluster, err := Combine(plans)
	if err != nil {
		return nil, err
	}
	for _, issue := range cluster.Issues {
		if issue.Code == "NODE_SNAPSHOT_MISSING" || issue.Code == "CONNECTION_DRIFT" {
			return nil, fmt.Errorf("migration blocked: %s (%s)", issue.Code, issue.Scope)
		}
	}
	for _, plan := range plans {
		for _, issue := range plan.Issues {
			if issue.Code == "UNSUPPORTED_PASSWORD_HASH" || issue.Code == "UNSUPPORTED_ROLE" || issue.Code == "NODE_IDENTITY_MISMATCH" || issue.Code == "PENDING_ENROLLMENT" {
				return nil, fmt.Errorf("migration blocked: %s (%s)", issue.Code, issue.Scope)
			}
		}
	}
	if err := p.prepareFoundation(ctx, options); err != nil {
		return nil, err
	}
	sort.Slice(p.Manifest.Nodes, func(i, j int) bool { return p.Manifest.Nodes[i].LegacyNode < p.Manifest.Nodes[j].LegacyNode })
	if err := writePrivateJSON(filepath.Join(staging, "manifest.json"), p.Manifest); err != nil {
		return nil, err
	}
	if err := writePrivateJSON(filepath.Join(staging, "identity-map.json"), p.Manifest.Nodes); err != nil {
		return nil, err
	}
	if err := syncDirectory(staging); err != nil {
		return nil, err
	}
	return p, nil
}

func copySnapshot(ctx context.Context, source, destination string) (string, error) {
	before, err := os.Lstat(source)
	if err != nil || !before.Mode().IsRegular() {
		return "", errors.New("snapshot must be an existing regular file, not a symlink")
	}
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", errors.New("snapshot changed while opening")
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := in.Read(buffer)
		if n > 0 {
			if _, err := out.Write(buffer[:n]); err != nil {
				return "", err
			}
			_, _ = h.Write(buffer[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	after, err := in.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("snapshot changed while copying")
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Iterate validates the private copy before invoking the converter. Keys and
// values are raw V4 bytes; callbacks must only write to the pending staging DBs.
func (p *Prepared) Iterate(ctx context.Context, node string, visit func(key, value []byte) error) error {
	if p.closed || visit == nil {
		return errors.New("migration preparation is closed or callback is missing")
	}
	path, found := p.paths[node]
	if !found {
		return errors.New("unknown legacy node")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	plan, err := Inspect(ctx, f)
	if err != nil {
		return err
	}
	if plan.Header != p.plans[node].Header || plan.Integrity != p.plans[node].Integrity {
		return errors.New("private migration snapshot changed")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = inspectRecords(ctx, f, visit)
	return err
}

func (p *Prepared) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	var errs []error
	if p.Master != nil {
		errs = append(errs, p.Master.Close())
	}
	for _, store := range p.Agents {
		errs = append(errs, store.Close())
	}
	return errors.Join(errs...)
}

func writePrivateJSON(path string, value any) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
