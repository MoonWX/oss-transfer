package servicehost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Options struct{ Name, Role, Version, ReadyFile, LogFile string }
type Program func(context.Context, func() error) error

var noncePattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type runtime struct {
	options          Options
	bootID, launchID string
	once             sync.Once
	readyErr         error
	log              *os.File
	stdout, stderr   *os.File
}

func prepare(options Options) (*runtime, error) {
	if !ValidName(options.Name) || (options.Role != "master" && options.Role != "agent") || !cleanValue(options.Version) {
		return nil, errors.New("invalid service identity")
	}
	if (options.ReadyFile != "" && !absolute(options.ReadyFile)) || (options.LogFile != "" && !absolute(options.LogFile)) {
		return nil, errors.New("service paths must be absolute")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	r := &runtime{options: options, bootID: hex.EncodeToString(nonce[:])}
	r.launchID = r.bootID
	if options.ReadyFile != "" {
		if err := secureDirectory(filepath.Dir(options.ReadyFile)); err != nil {
			return nil, err
		}
		if err := regularOrAbsent(options.ReadyFile); err != nil {
			return nil, err
		}
		if err := os.Remove(options.ReadyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		request := options.ReadyFile + ".request"
		if err := regularOrAbsent(request); err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(request)
		if err == nil {
			r.launchID = strings.TrimSpace(string(raw))
			if !noncePattern.MatchString(r.launchID) {
				return nil, errors.New("invalid startup request")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if options.LogFile != "" {
		if err := secureDirectory(filepath.Dir(options.LogFile)); err != nil {
			return nil, err
		}
		if err := regularOrAbsent(options.LogFile); err != nil {
			return nil, err
		}
		log, err := os.OpenFile(options.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, err
		}
		if err = privateFile(log); err != nil {
			log.Close()
			return nil, err
		}
		r.log, r.stdout, r.stderr = log, os.Stdout, os.Stderr
		os.Stdout, os.Stderr = log, log
	}
	return r, nil
}
func regularOrAbsent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("expected a regular service file: %s", path)
	}
	return nil
}
func secureDirectory(path string) error {
	for ancestor := path; ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Lstat(ancestor)
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			// macOS ships these root-owned aliases. No application-controlled link is accepted.
			if goruntime.GOOS == "darwin" && (ancestor == "/var" || ancestor == "/tmp" || ancestor == "/etc") {
				target, linkErr := os.Readlink(ancestor)
				if linkErr == nil && (target == "private"+ancestor || target == "/private"+ancestor) {
					continue
				}
			}
			return errors.New("service directory contains a link or non-directory")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return privateDirectory(path)
}
func (r *runtime) ready() error {
	r.once.Do(func() {
		if r.options.ReadyFile == "" {
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "format\toss-transfer-ready-v1\npid\t%d\nbootId\t%s\nlaunchId\t%s\nrole\t%s\nversion\t%s\nreadyAt\t%s\n", os.Getpid(), r.bootID, r.launchID, r.options.Role, r.options.Version, time.Now().UTC().Format(time.RFC3339Nano))
		f, err := os.CreateTemp(filepath.Dir(r.options.ReadyFile), ".ready-*")
		if err != nil {
			r.readyErr = err
			return
		}
		defer os.Remove(f.Name())
		if err = privateFile(f); err == nil {
			_, err = io.WriteString(f, b.String())
		}
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err == nil {
			err = os.Rename(f.Name(), r.options.ReadyFile)
		}
		r.readyErr = err
	})
	return r.readyErr
}
func (r *runtime) close() {
	if r.options.ReadyFile != "" {
		// Never remove another process's marker after a supervisor restart.
		raw, err := os.ReadFile(r.options.ReadyFile)
		if err == nil && strings.Contains(string(raw), "\nbootId\t"+r.bootID+"\n") {
			_ = os.Remove(r.options.ReadyFile)
		}
	}
	if r.log != nil {
		os.Stdout, os.Stderr = r.stdout, r.stderr
		_ = r.log.Close()
	}
}

// The supervisor may not retain stderr. Write failures before restoring the
// console handles so a background service's startup error survives shutdown.
func (r *runtime) finish(err error) {
	if err != nil && !errors.Is(err, context.Canceled) && r.log != nil {
		_, _ = fmt.Fprintf(r.log, "Service failed: %v\n", err)
		_ = r.log.Sync()
	}
	r.close()
}

// CheckReady validates the invocation nonce, process ID, role and release. The
// installer obtains PID from the service manager, not from this marker.
func CheckReady(path, launchID, role, version string, pid int) error {
	if !noncePattern.MatchString(launchID) || pid < 1 {
		return errors.New("invalid readiness expectation")
	}
	if err := regularOrAbsent(path); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) > 4096 {
		return errors.New("readiness record is too large")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "\t")
		_, duplicate := fields[k]
		if !ok || duplicate || v == "" {
			return errors.New("invalid readiness record")
		}
		fields[k] = v
	}
	if len(fields) != 7 || fields["format"] != "oss-transfer-ready-v1" || fields["pid"] != strconv.Itoa(pid) || fields["launchId"] != launchID || fields["role"] != role || fields["version"] != version || !noncePattern.MatchString(fields["bootId"]) {
		return errors.New("service has not confirmed this startup")
	}
	return nil
}
