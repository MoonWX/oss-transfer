package transferstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

func (s *S3) readPart(ctx context.Context, reader io.Reader, size, memoryLimit int64) (*partBody, error) {
	if size <= memoryLimit || s.tempRegistry == nil {
		return readPartWithLimit(reader, size, s.spoolDirectory, memoryLimit)
	}
	body, err := newTrackedSpool(ctx, s.spoolDirectory, s.tempRegistry)
	if err != nil {
		return nil, err
	}
	count, err := io.CopyBuffer(body.file, io.LimitReader(reader, size), make([]byte, 256*1024))
	if err == nil && count != size {
		err = fmt.Errorf("%w: source shorter than declared length", ErrSizeMismatch)
	}
	if err == nil {
		_, err = body.file.Seek(0, io.SeekStart)
	}
	if err != nil {
		return nil, errors.Join(err, body.close())
	}
	return body, nil
}

func newTrackedSpool(ctx context.Context, spool string, registry LocalTempRegistry) (*partBody, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validHex(registry.OwnerID(), 32) {
		return nil, ErrTempIdentity
	}
	if spool == "" {
		spool = os.TempDir()
	}
	absolute, err := filepath.Abs(spool)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	directory, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		directory.Close()
		return nil, err
	}
	id := hex.EncodeToString(nonce[:])
	name := trackedTempName(registry.OwnerID(), id)
	file, err := directory.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		directory.Close()
		return nil, err
	}
	registered := false
	var once sync.Once
	var cleanupError error
	body := &partBody{reader: file, file: file, name: filepath.Join(absolute, name)}
	body.cleanup = func() error {
		once.Do(func() {
			closeErr := file.Close()
			removeErr := directory.Remove(name)
			if errors.Is(removeErr, fs.ErrNotExist) {
				removeErr = nil
			}
			var forgetErr error
			if removeErr == nil && registered {
				forgetErr = registry.Forget(context.WithoutCancel(ctx), id)
			}
			cleanupError = errors.Join(closeErr, removeErr, forgetErr, directory.Close())
		})
		return cleanupError
	}
	parentInfo, err := directory.Stat(".")
	if err != nil {
		return nil, errors.Join(err, body.close())
	}
	parentIdentity, err := storageIdentity(directory, ".", parentInfo)
	if err != nil {
		return nil, errors.Join(err, body.close())
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, body.close())
	}
	identity, err := openedIdentity(file, info)
	if err != nil {
		return nil, errors.Join(err, body.close())
	}
	record := LocalTempRecord{Owner: registry.OwnerID(), ID: id, ParentPath: absolute, ParentIdentity: parentIdentity, Name: name, FileIdentity: identity}
	if err := registry.Register(ctx, record); err != nil {
		return nil, errors.Join(err, body.close())
	}
	registered = true
	return body, nil
}
