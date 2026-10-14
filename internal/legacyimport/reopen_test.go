package legacyimport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestReopenPendingMigrationRevalidatesAndKeepsOriginalDatabase(t *testing.T) {
	f := taskFixture(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ConvertTasks(context.Background(), TaskImportOptions{}); err != nil {
		t.Fatal(err)
	}
	directory := p.Directory
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = OpenPrepared(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	status, err := p.Status()
	if err != nil || status.TaskCatalogue == nil || !status.TaskCatalogue.CatalogueComplete || !status.Pending {
		t.Fatal(status, err)
	}
	if _, err := p.ConvertTasks(context.Background(), TaskImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := task.New(p.Master).Get(224); err != nil {
		t.Fatal(err)
	}
	if err := p.Master.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(directory, "master", "master.db")
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(directory, "raw", "00001.jsonl")
	file, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("\n")
	file.Close()
	if bad, err := OpenPrepared(context.Background(), directory); err == nil {
		bad.Close()
		t.Fatal("corrupted private snapshot reopened")
	}
	after, err := os.Stat(dbPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("failed reopen replaced DB", err)
	}
}
func TestReopenNeverCreatesMissingDatabaseOrFollowsDirectoryLinks(t *testing.T) {
	f := prepareFixture(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	directory := p.Directory
	p.Close()
	dbPath := filepath.Join(directory, "master", "master.db")
	if err := os.Rename(dbPath, dbPath+".saved"); err != nil {
		t.Fatal(err)
	}
	if bad, err := OpenPrepared(context.Background(), directory); err == nil {
		bad.Close()
		t.Fatal("missing DB accepted")
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reopen created DB", err)
	}
	if err := os.Rename(dbPath+".saved", dbPath); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(directory, "master")
	moved := filepath.Join(directory, "old-master")
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, original); err != nil {
		t.Fatal(err)
	}
	if bad, err := OpenPrepared(context.Background(), directory); err == nil {
		bad.Close()
		t.Fatal("linked DB directory accepted")
	}
}
