package servicehost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedCLIConnectionUsesManagedCLIAndContainsNoCredentials(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			dir := t.TempDir()
			binary, cli := filepath.Join(dir, "oss-transfer-agent"), filepath.Join(dir, "oss-transfer-cli")
			if err := os.WriteFile(cli, []byte("cli"), 0755); err != nil {
				t.Fatal(err)
			}
			config := Config{ServiceName: "agent-test", DataDirectory: filepath.Join(dir, "data")}
			if err := os.Mkdir(config.DataDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			fields := map[string]string{"role": "agent", "serviceName": config.ServiceName, "dataDir": config.DataDirectory, "cliPath": cli}
			var raw []byte
			name := "install.tsv"
			if platform == "windows" {
				name = "install.json"
				raw, _ = json.Marshal(fields)
			} else {
				for key, value := range fields {
					raw = append(raw, []byte(key+"\t"+value+"\n")...)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := publishCLIConnection(binary, platform, "agent", "http://10.1.0.43:1688", config); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(cli + ".cluster.json")
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if json.Unmarshal(data, &got) != nil || len(got) != 2 || got["masterUrl"] != "http://10.1.0.43:1688" || got["role"] != "agent" {
				t.Fatalf("unexpected public data: %s", data)
			}
			if strings.Contains(string(data), config.DataDirectory) {
				t.Fatal("unnecessary service data exposed")
			}
			config.ServiceName = "other-agent"
			if err := publishCLIConnection(binary, platform, "agent", "http://other:1685", config); err == nil {
				t.Fatal("mismatched installation overwritten")
			}
		})
	}
}

func TestLocalMasterOriginPreservesPortAndAddressFamily(t *testing.T) {
	for listen, want := range map[string]string{":1688": "http://127.0.0.1:1688", "0.0.0.0:1688": "http://127.0.0.1:1688", "[::]:1688": "http://[::1]:1688", "10.0.0.1:9000": "http://10.0.0.1:9000"} {
		got, err := LocalMasterOrigin(listen)
		if err != nil || got != want {
			t.Fatal(listen, got, err)
		}
	}
}

func TestContainerPublishesConnectionWithoutWritingToExecutableDirectory(t *testing.T) {
	data := t.TempDir()
	config := Config{DataDirectory: data}
	if err := publishCLIConnection(filepath.Join(t.TempDir(), "oss-transfer-agent"), "linux", "agent", "https://cluster.example", config); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(data, "cli-cluster.json"))
	if err != nil || !strings.Contains(string(raw), `"masterUrl":"https://cluster.example"`) {
		t.Fatal(string(raw), err)
	}
}
