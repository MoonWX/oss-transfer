package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/master"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestResetAdministratorRequiresStoppedExistingDatabase(t *testing.T) {
	directory := t.TempDir()
	if err := resetAdministrator(directory, ""); err == nil {
		t.Fatal("recovery created a missing installation")
	}
	if _, err := os.Stat(filepath.Join(directory, "master.db")); !os.IsNotExist(err) {
		t.Fatal("recovery created a database")
	}
	db, err := state.Open(filepath.Join(directory, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	server, _, err := master.New(db, master.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := resetAdministrator(directory, ""); err == nil || !strings.Contains(err.Error(), "stop the Master") {
		t.Fatal("recovery did not require the exclusive database lock", err)
	}
	_ = server.Close()
	_ = db.Close()
	previous := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	os.Stdout = writer
	defer func() { os.Stdout = previous }()
	err = resetAdministrator(directory, "")
	_ = writer.Close()
	os.Stdout = previous
	if err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 || lines[0] != "Initial administrator: admin" || !strings.HasPrefix(lines[1], "Initial password: ") {
		t.Fatal("recovery did not print login credentials")
	}
}
