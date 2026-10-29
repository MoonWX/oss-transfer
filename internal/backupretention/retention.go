// Package backupretention removes only completed rollback backups belonging to
// one native installation. Old, incomplete and foreign backups are never adopted.
package backupretention

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	MarkerName        = "backup.json"
	LockName          = ".retention.lock"
	ActiveName        = ".active"
	DefaultKeep       = 2
	DefaultMaxAgeDays = 30
)

var (
	ErrBusy     = errors.New("backup maintenance is already running")
	ErrUnsafe   = errors.New("unsafe backup path or contents")
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,199}$`)
)

type Identity struct {
	Role        string `json:"role"`
	ServiceName string `json:"serviceName"`
	Config      string `json:"config"`
	DataDir     string `json:"dataDir"`
}

type Policy struct {
	Keep       int `json:"keep"`
	MaxAgeDays int `json:"maxAgeDays"`
}

// Names is for trusted maintenance callers only. Explicit selection still
// protects the newest usable backup, active backups and all unrecognized data.
// A zero Keep or MaxAgeDays uses the default; negative values are rejected.
type Options struct {
	Keep       int
	MaxAgeDays int
	Names      []string
}

type Entry struct {
	Name        string    `json:"name"`
	Version     string    `json:"version,omitempty"`
	CompletedAt time.Time `json:"completedAt,omitempty"`
	SizeBytes   int64     `json:"sizeBytes"`
	Managed     bool      `json:"managed"`
	Protected   bool      `json:"protected"`
	Reason      string    `json:"reason,omitempty"`
	Eligible    bool      `json:"eligible"`
	info        fs.FileInfo
}

type Listing struct {
	Root             string  `json:"root"`
	Entries          []Entry `json:"entries"`
	TotalBytes       int64   `json:"totalBytes"`
	ReclaimableBytes int64   `json:"reclaimableBytes"`
	Policy           Policy  `json:"policy"`
}

type Result struct {
	Removed []string `json:"removed"`
	// FreedBytes is the total logical file size of wholly removed backups.
	FreedBytes int64   `json:"freedBytes"`
	Skipped    []Entry `json:"skipped"`
}

type marker struct {
	Schema      int       `json:"schema"`
	State       string    `json:"state"`
	Identity    Identity  `json:"identity"`
	Version     string    `json:"version"`
	CompletedAt time.Time `json:"completedAt"`
}

type Manager struct {
	root        string
	identity    Identity
	now         func() time.Time
	lockTimeout time.Duration
	removeAll   func(*os.Root, string) error
}

func New(root string, identity Identity) (*Manager, error) {
	if (identity.Role != "master" && identity.Role != "agent") || !namePattern.MatchString(identity.ServiceName) ||
		!absolute(identity.Config) || !absolute(identity.DataDir) || !absolute(root) {
		return nil, errors.New("backup root and installation identity must use absolute, clean paths")
	}
	root = systemAlias(root)
	data, config := systemAlias(identity.DataDir), systemAlias(filepath.Dir(identity.Config))
	if root == filepath.Dir(root) || contains(root, data) || contains(data, root) || contains(root, config) || contains(config, root) {
		return nil, errors.New("backup directory must be separate from installation configuration and data")
	}
	if err := safeDirectories(root); err != nil {
		return nil, err
	}
	return &Manager{root: root, identity: identity, now: time.Now, lockTimeout: time.Second,
		removeAll: func(root *os.Root, name string) error { return root.RemoveAll(name) }}, nil
}

func absolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n\t")
}

func contains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// These are OS-provided aliases on macOS, not user-controlled backup links.
func systemAlias(path string) string {
	if runtime.GOOS == "darwin" {
		for _, prefix := range []string{"/var", "/tmp", "/etc"} {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return "/private" + path
			}
		}
	}
	return path
}

func safeDirectories(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && !info.IsDir() {
			return fmt.Errorf("%w: directory %s", ErrUnsafe, current)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

// lock pins the directory and opens bbolt's lock through os.Root, so replacing a
// path component cannot redirect maintenance outside the selected directory.
func (m *Manager) lock() (*os.Root, *bolt.DB, error) {
	if err := safeDirectories(m.root); err != nil {
		return nil, nil, err
	}
	before, err := os.Lstat(m.root)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(m.root)
	if err != nil {
		return nil, nil, err
	}
	opened, openedErr := root.Stat(".")
	after, afterErr := os.Lstat(m.root)
	if openedErr != nil || afterErr != nil || !after.IsDir() || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
		root.Close()
		return nil, nil, ErrUnsafe
	}
	if err = safeDirectories(m.root); err != nil {
		root.Close()
		return nil, nil, err
	}
	db, err := bolt.Open(filepath.Join(m.root, LockName), 0600, &bolt.Options{
		Timeout: m.lockTimeout,
		OpenFile: func(_ string, flags int, mode os.FileMode) (*os.File, error) {
			if info, err := root.Lstat(LockName); err == nil && !info.Mode().IsRegular() {
				return nil, ErrUnsafe
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			file, err := root.OpenFile(LockName, flags, mode)
			if err != nil {
				return nil, err
			}
			pathInfo, pathErr := root.Lstat(LockName)
			fileInfo, fileErr := file.Stat()
			if pathErr != nil || fileErr != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
				file.Close()
				return nil, ErrUnsafe
			}
			return file, nil
		},
	})
	if err != nil {
		root.Close()
		if errors.Is(err, bolt.ErrTimeout) {
			err = ErrBusy
		}
		return nil, nil, err
	}
	return root, db, nil
}

func policy(options Options) (Policy, error) {
	if options.Keep < 0 || options.MaxAgeDays < 0 || options.MaxAgeDays > 36500 {
		return Policy{}, errors.New("invalid backup retention policy")
	}
	p := Policy{Keep: options.Keep, MaxAgeDays: options.MaxAgeDays}
	if p.Keep == 0 {
		p.Keep = DefaultKeep
	}
	if p.MaxAgeDays == 0 {
		p.MaxAgeDays = DefaultMaxAgeDays
	}
	return p, nil
}

func (m *Manager) List() (Listing, error) {
	p, _ := policy(Options{})
	listing := Listing{Root: m.root, Entries: []Entry{}, Policy: p}
	root, lock, err := m.lock()
	if errors.Is(err, os.ErrNotExist) {
		return listing, nil
	}
	if err != nil {
		return listing, err
	}
	defer root.Close()
	defer lock.Close()
	return m.list(root, p)
}

func (m *Manager) list(root *os.Root, p Policy) (Listing, error) {
	listing := Listing{Root: m.root, Entries: []Entry{}, Policy: p}
	directory, err := root.Open(".")
	if err != nil {
		return listing, err
	}
	children, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return listing, err
	}
	for _, child := range children {
		if child.Name() == LockName {
			continue
		}
		entry := m.inspect(root, child.Name())
		listing.TotalBytes += entry.SizeBytes
		listing.Entries = append(listing.Entries, entry)
	}
	sort.Slice(listing.Entries, func(i, j int) bool {
		a, b := listing.Entries[i], listing.Entries[j]
		if !a.CompletedAt.Equal(b.CompletedAt) {
			return a.CompletedAt.After(b.CompletedAt)
		}
		return a.Name > b.Name
	})
	position := 0
	for index := range listing.Entries {
		entry := &listing.Entries[index]
		if !entry.Managed || entry.Protected {
			continue
		}
		if position == 0 {
			entry.Protected, entry.Reason = true, "LATEST"
		} else if position >= p.Keep || entry.CompletedAt.Before(m.now().AddDate(0, 0, -p.MaxAgeDays)) {
			entry.Eligible = true
			listing.ReclaimableBytes += entry.SizeBytes
		}
		position++
	}
	return listing, nil
}

func (m *Manager) inspect(root *os.Root, name string) Entry {
	entry := Entry{Name: name, Protected: true, Reason: "UNMANAGED"}
	info, err := root.Lstat(name)
	if err != nil {
		entry.Reason = "UNREADABLE"
		return entry
	}
	entry.info = info
	if !info.IsDir() || !validName(name) {
		entry.Reason = "UNSAFE"
		if info.Mode().IsRegular() {
			entry.SizeBytes = info.Size()
		}
		return entry
	}
	directory, err := openDirectory(root, name, info)
	if err != nil {
		entry.Reason = "UNSAFE"
		return entry
	}
	defer directory.Close()
	entry.SizeBytes, err = treeSize(directory)
	if err != nil {
		entry.Reason = "UNSAFE"
		return entry
	}
	if _, err = directory.Lstat(ActiveName); err == nil {
		entry.Reason = "ACTIVE"
		return entry
	} else if !errors.Is(err, os.ErrNotExist) {
		entry.Reason = "UNREADABLE"
		return entry
	}
	raw, err := readRegular(directory, MarkerName)
	if errors.Is(err, os.ErrNotExist) {
		return entry
	}
	if err != nil {
		entry.Reason = "INVALID"
		return entry
	}
	var completed marker
	if err = json.Unmarshal(raw, &completed); err != nil || completed.Schema != 1 || completed.State != "COMPLETED" || completed.CompletedAt.IsZero() || completed.CompletedAt.After(m.now().Add(5*time.Minute)) {
		entry.Reason = "INVALID"
		return entry
	}
	if completed.Identity != m.identity {
		entry.Reason = "OTHER_INSTALLATION"
		return entry
	}
	version, err := m.validateBackup(directory)
	if err != nil || version != completed.Version {
		entry.Reason = "INVALID"
		return entry
	}
	entry.Version, entry.CompletedAt = version, completed.CompletedAt
	entry.Managed, entry.Protected, entry.Reason = true, false, ""
	return entry
}

func validName(name string) bool {
	return namePattern.MatchString(name) && name != "." && name != ".." && !strings.ContainsAny(name, `/\\:`)
}

func openDirectory(root *os.Root, name string, before fs.FileInfo) (*os.Root, error) {
	directory, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, openedErr := directory.Stat(".")
	after, afterErr := root.Lstat(name)
	if openedErr != nil || afterErr != nil || !after.IsDir() || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
		directory.Close()
		return nil, ErrUnsafe
	}
	return directory, nil
}

func treeSize(directory *os.Root) (int64, error) {
	var size int64
	err := fs.WalkDir(directory.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return ErrUnsafe
		}
		size += info.Size()
		return nil
	})
	return size, err
}

func readRegular(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, ErrUnsafe
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, ErrUnsafe
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if len(raw) > 65536 {
		return nil, ErrUnsafe
	}
	return raw, err
}

func (m *Manager) validateBackup(root *os.Root) (string, error) {
	metadataName, binaryName := "install.tsv", "binary"
	if runtime.GOOS == "windows" {
		metadataName, binaryName = "install.json", "binary.exe"
	}
	for _, name := range []string{binaryName, "database"} {
		info, err := root.Lstat(name)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return "", ErrUnsafe
		}
	}
	raw, err := readRegular(root, "service.json")
	if err != nil {
		return "", err
	}
	var config struct {
		ServiceName   string `json:"serviceName"`
		DataDirectory string `json:"dataDirectory"`
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		return "", err
	}
	if config.ServiceName != m.identity.ServiceName || config.DataDirectory != m.identity.DataDir {
		return "", errors.New("backup service identity does not match")
	}
	raw, err = readRegular(root, metadataName)
	if err != nil {
		return "", err
	}
	var installed struct {
		Identity
		Version string `json:"version"`
	}
	if metadataName == "install.json" {
		if err = json.Unmarshal(raw, &installed); err != nil {
			return "", err
		}
	} else {
		fields := map[string]string{}
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			key, value, ok := strings.Cut(line, "\t")
			if _, duplicate := fields[key]; !ok || duplicate || strings.ContainsAny(value, "\r\n\t\x00") {
				return "", errors.New("invalid installation metadata")
			}
			fields[key] = value
		}
		installed.Identity = Identity{Role: fields["role"], ServiceName: fields["serviceName"], Config: fields["config"], DataDir: fields["dataDir"]}
		installed.Version = fields["version"]
	}
	// Early managed installations omitted dataDir; service.json still provides
	// the exact data directory and is independently checked above.
	if installed.DataDir == "" {
		installed.DataDir = config.DataDirectory
	}
	if installed.Identity != m.identity || installed.Version == "" || strings.ContainsAny(installed.Version, "\r\n\t\x00") {
		return "", errors.New("backup installation identity does not match")
	}
	return installed.Version, nil
}

// Complete records a rollback backup only after the replacement service passed
// its health check. version identifies the OLD backed-up program, never the new
// installed program. Empty version is read from the saved install metadata.
func (m *Manager) Complete(name, version string) error {
	if !validName(name) {
		return ErrUnsafe
	}
	root, lock, err := m.lock()
	if err != nil {
		return err
	}
	defer root.Close()
	defer lock.Close()
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrUnsafe
	}
	directory, err := openDirectory(root, name, info)
	if err != nil {
		return err
	}
	defer directory.Close()
	if _, err = treeSize(directory); err != nil {
		return err
	}
	if _, err = directory.Lstat(ActiveName); err == nil {
		return errors.New("backup is still active")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	savedVersion, err := m.validateBackup(directory)
	if err != nil {
		return err
	}
	if version != "" && version != savedVersion {
		return errors.New("version differs from the backed-up installation")
	}
	if raw, readErr := readRegular(directory, MarkerName); readErr == nil {
		var existing marker
		if json.Unmarshal(raw, &existing) == nil && existing.Schema == 1 && existing.State == "COMPLETED" && existing.Identity == m.identity && existing.Version == savedVersion && !existing.CompletedAt.IsZero() {
			return nil
		}
		return errors.New("backup already has a different or unsuccessful completion marker")
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	completed := marker{Schema: 1, State: "COMPLETED", Identity: m.identity, Version: savedVersion, CompletedAt: m.now().UTC()}
	raw, err := json.Marshal(completed)
	if err != nil {
		return err
	}
	const pending = ".backup-complete.tmp"
	file, err := directory.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer directory.Remove(pending)
	_, writeErr := file.Write(append(raw, '\n'))
	syncErr, closeErr := file.Sync(), file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return directory.Rename(pending, MarkerName)
}

func (m *Manager) Prune(options Options) (Result, error) {
	result := Result{Removed: []string{}, Skipped: []Entry{}}
	p, err := policy(options)
	if err != nil {
		return result, err
	}
	selected := make(map[string]bool, len(options.Names))
	for _, name := range options.Names {
		if !validName(name) {
			return result, ErrUnsafe
		}
		selected[name] = true
	}
	root, lock, err := m.lock()
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer root.Close()
	defer lock.Close()
	listing, err := m.list(root, p)
	if err != nil {
		return result, err
	}
	var failures []error
	for _, entry := range listing.Entries {
		if len(selected) > 0 && !selected[entry.Name] {
			continue
		}
		if !entry.Managed || entry.Protected || (len(selected) == 0 && !entry.Eligible) {
			result.Skipped = append(result.Skipped, entry)
			continue
		}
		// Revalidate immediately before removal; another actor not using our
		// maintenance lock may have replaced or marked the directory active.
		current := m.inspect(root, entry.Name)
		if !current.Managed || current.Protected || !os.SameFile(entry.info, current.info) {
			result.Skipped = append(result.Skipped, current)
			continue
		}
		if err = m.removeAll(root, entry.Name); err != nil {
			failures = append(failures, fmt.Errorf("remove backup %s: %w", entry.Name, err))
			continue
		}
		if _, err = root.Lstat(entry.Name); !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("backup %s still exists after cleanup", entry.Name))
			continue
		}
		result.Removed = append(result.Removed, entry.Name)
		result.FreedBytes += current.SizeBytes
	}
	return result, errors.Join(failures...)
}
