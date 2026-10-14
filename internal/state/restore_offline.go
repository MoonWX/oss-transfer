package state

import (
	"errors"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

// RestoreOffline restores logical contents under the original file's exclusive
// bbolt lock. It never replaces the file: another process cannot keep writing an
// unlinked database while a new service opens a restored copy at the same path.
func RestoreOffline(database, role, backup string) error {
	if role != "master" && role != "agent" {
		return errors.New("invalid database role")
	}
	currentInfo, err := offlineFileInfo(database)
	if err != nil {
		return err
	}
	backupInfo, err := offlineFileInfo(backup)
	if err != nil {
		return err
	}
	if os.SameFile(currentInfo, backupInfo) {
		return errors.New("backup and database must be different files")
	}
	snapshot, err := openExistingOffline(backup, backupInfo, true)
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	defer snapshot.Close()
	return snapshot.View(func(source *bolt.Tx) error {
		// A snapshot retains the final destination fence; its temporary backup
		// filename must never become a new allowed installation location.
		if err := validateOfflineTransaction(source, role, database); err != nil {
			return fmt.Errorf("invalid backup: %w", err)
		}
		current, err := openExistingOffline(database, currentInfo, false)
		if err != nil {
			return fmt.Errorf("open offline database (stop every process using it first): %w", err)
		}
		defer current.Close()
		return current.Update(func(destination *bolt.Tx) error {
			if err := validateOfflineTransaction(destination, role, database); err != nil {
				return fmt.Errorf("invalid database: %w", err)
			}
			return restoreTransaction(destination, source)
		})
	})
}

func offlineFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("existing database must be a nonempty regular file")
	}
	return info, nil
}

func openExistingOffline(path string, original os.FileInfo, readOnly bool) (*bolt.DB, error) {
	return bolt.Open(path, 0600, &bolt.Options{
		ReadOnly: readOnly, Timeout: time.Second,
		OpenFile: func(name string, flag int, mode os.FileMode) (*os.File, error) {
			// bbolt normally opens writable databases with O_CREATE. A missing
			// original is an error during rollback, never a new blank database.
			file, err := os.OpenFile(name, flag&^(os.O_CREATE|os.O_TRUNC), mode)
			if err != nil {
				return nil, err
			}
			opened, err := file.Stat()
			if err != nil {
				file.Close()
				return nil, err
			}
			current, err := offlineFileInfo(name)
			if err != nil {
				file.Close()
				return nil, err
			}
			if !os.SameFile(original, opened) || !os.SameFile(current, opened) {
				file.Close()
				return nil, errors.New("database file changed while opening it")
			}
			return file, nil
		},
	})
}

func restoreTransaction(destination, source *bolt.Tx) error {
	var names [][]byte
	if err := destination.ForEach(func(name []byte, _ *bolt.Bucket) error {
		names = append(names, append([]byte(nil), name...))
		return nil
	}); err != nil {
		return err
	}
	for _, name := range names {
		if err := destination.DeleteBucket(name); err != nil {
			return err
		}
	}
	return source.ForEach(func(name []byte, original *bolt.Bucket) error {
		copy, err := destination.CreateBucket(name)
		if err != nil {
			return err
		}
		return restoreBucket(copy, original)
	})
}

func restoreBucket(destination, source *bolt.Bucket) error {
	if err := destination.SetSequence(source.Sequence()); err != nil {
		return err
	}
	return source.ForEach(func(key, value []byte) error {
		if nested := source.Bucket(key); nested != nil {
			copy, err := destination.CreateBucket(key)
			if err != nil {
				return err
			}
			return restoreBucket(copy, nested)
		}
		return destination.Put(key, value)
	})
}
