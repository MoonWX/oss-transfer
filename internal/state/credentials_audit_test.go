package state

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// This opt-in operational check is compiled only into tests. Run it on a stopped
// database (or a coherent offline snapshot). It never prints credential values.
func TestAuditEncryptedCredentialDatabase(t *testing.T) {
	path := os.Getenv("OSS_TRANSFER_CREDENTIAL_AUDIT_DB")
	if path == "" {
		t.Skip("set OSS_TRANSFER_CREDENTIAL_AUDIT_DB to an offline database")
	}
	info, err := offlineFileInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	database, err := openExistingOffline(path, info, true)
	if err != nil {
		t.Fatal("stop the service before auditing its database", err)
	}
	defer database.Close()
	var needles [][]byte
	if err := database.View(func(raw *bolt.Tx) error {
		credentials, err := offlineCredentialCipher(raw, path)
		if err != nil {
			return err
		}
		if credentials == nil {
			t.Fatal("database credentials have not been encrypted")
		}
		tx := &Tx{tx: raw, credentials: credentials}
		return tx.Scan(credentialBucket, "", "", 1000, func(_ string, data json.RawMessage) error {
			var snapshot struct {
				Connections []struct {
					AccessKey    string `json:"accessKey"`
					SecretKey    string `json:"secretKey"`
					SessionToken string `json:"sessionToken"`
				} `json:"connections"`
			}
			if err := json.Unmarshal(data, &snapshot); err != nil {
				return err
			}
			for _, connection := range snapshot.Connections {
				for _, secret := range []string{connection.AccessKey, connection.SecretKey, connection.SessionToken} {
					if secret != "" {
						needles = append(needles, []byte(secret))
					}
				}
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if len(needles) == 0 {
		t.Fatal("database has no configured credential values to audit")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	overlap := 0
	for _, needle := range needles {
		overlap = max(overlap, len(needle)-1)
	}
	buffer := make([]byte, (1<<20)+overlap)
	kept := 0
	for {
		n, err := file.Read(buffer[kept:])
		data := buffer[:kept+n]
		for _, needle := range needles {
			if bytes.Contains(data, needle) {
				t.Fatal("credential bytes remain in the database file; compact it before claiming physical removal")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kept = min(overlap, len(data))
		copy(buffer[:kept], data[len(data)-kept:])
	}
	t.Logf("Validated encrypted records and private key; %d credential values absent from the entire database file", len(needles))
}
