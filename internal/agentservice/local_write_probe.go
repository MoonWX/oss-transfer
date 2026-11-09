package agentservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Write probe outcomes in WriteProbe.ErrorCode.
const (
	WriteProbePermissionDenied = "PERMISSION_DENIED"
	WriteProbeReadOnly         = "READ_ONLY"
	WriteProbeNoSpace          = "NO_SPACE"
	WriteProbeNotDirectory     = "NOT_DIRECTORY"
	WriteProbeFailed           = "FAILED"
)

// WriteProbe reports whether this Agent can create files in a download
// destination. A destination that does not exist yet is checked in its
// nearest existing ancestor, where the task would create it.
type WriteProbe struct {
	Path           string `json:"path"`
	ProbeDirectory string `json:"probeDirectory,omitempty"`
	Writable       bool   `json:"writable"`
	ErrorCode      string `json:"errorCode,omitempty"`
	Error          string `json:"error,omitempty"`
	// EffectiveUID is the user the Agent writes as; absent on Windows.
	EffectiveUID *int `json:"effectiveUid,omitempty"`
	// FreeBytes and TotalBytes describe the probed directory's file system,
	// when the system reports them.
	FreeBytes  *int64 `json:"freeBytes,omitempty"`
	TotalBytes *int64 `json:"totalBytes,omitempty"`
}

const writeProbePrefix = ".oss-transfer-write-probe-"

// probeLocalWrite creates and removes a small file in path or its nearest
// existing ancestor. Failures are reported in the result, not as an error;
// only a cancelled request returns one.
func probeLocalWrite(ctx context.Context, path string) (WriteProbe, error) {
	probe := WriteProbe{Path: path}
	if uid := os.Geteuid(); uid >= 0 {
		probe.EffectiveUID = &uid
	}
	directory := filepath.Clean(path)
	for {
		if err := ctx.Err(); err != nil {
			return probe, err
		}
		info, err := statPreflightPath(ctx, directory)
		if err == nil {
			if !info.IsDir() {
				probe.ProbeDirectory = directory
				probe.ErrorCode, probe.Error = WriteProbeNotDirectory, (&fs.PathError{Op: "probe", Path: directory, Err: syscall.ENOTDIR}).Error()
				return probe, nil
			}
			break
		}
		parent := filepath.Dir(directory)
		if !errors.Is(err, fs.ErrNotExist) || parent == directory {
			probe.ProbeDirectory = directory
			probe.ErrorCode, probe.Error = writeProbeCode(err), err.Error()
			return probe, nil
		}
		directory = parent
	}
	probe.ProbeDirectory = directory
	if free, total, ok := diskSpace(directory); ok {
		probe.FreeBytes, probe.TotalBytes = &free, &total
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return probe, err
	}
	name := filepath.Join(directory, writeProbePrefix+hex.EncodeToString(nonce[:]))
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		probe.ErrorCode, probe.Error = writeProbeCode(err), err.Error()
		return probe, nil
	}
	_, writeErr := io.WriteString(file, "oss-transfer write probe\n")
	closeErr := file.Close()
	removeErr := os.Remove(name)
	if err := errors.Join(writeErr, closeErr, removeErr); err != nil {
		probe.ErrorCode, probe.Error = writeProbeCode(err), err.Error()
		return probe, nil
	}
	probe.Writable = true
	return probe, nil
}

func writeProbeCode(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return WriteProbePermissionDenied
	case errors.Is(err, syscall.EROFS):
		return WriteProbeReadOnly
	case errors.Is(err, syscall.ENOSPC):
		return WriteProbeNoSpace
	case errors.Is(err, syscall.ENOTDIR):
		return WriteProbeNotDirectory
	}
	return WriteProbeFailed
}
