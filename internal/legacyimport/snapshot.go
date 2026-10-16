// Package legacyimport validates offline V4 exports and describes conversion
// requirements. It does not create or change a running Master or Agent database.
package legacyimport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxLineBytes = 92 << 20
const maxFacts = 100_000

type Header struct {
	Type             string    `json:"type"`
	SchemaVersion    int       `json:"schemaVersion"`
	Node             string    `json:"node"`
	Role             string    `json:"role"`
	LegacyJarSHA256  string    `json:"legacyJarSha256"`
	RocksDBJNISHA256 string    `json:"rocksdbJniSha256"`
	ExportedAt       time.Time `json:"exportedAt"`
}

type Footer struct {
	Type       string `json:"type"`
	Entries    int64  `json:"entries"`
	KeyBytes   int64  `json:"keyBytes"`
	ValueBytes int64  `json:"valueBytes"`
	SHA256     string `json:"sha256"`
}

type Issue struct {
	Code    string `json:"code"`
	Scope   string `json:"scope,omitempty"`
	Message string `json:"message"`
}

type UserPlan struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	HashKind string `json:"hashKind"`
}

type ConnectionPlan struct {
	Alias       string `json:"alias"`
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region"`
	Signature   string `json:"signature"`
	PathStyle   bool   `json:"pathStyle"`
	fingerprint string
}

type TaskPlan struct {
	LegacyKey       string `json:"legacyKey"`
	TaskID          int64  `json:"taskId"`
	ParentID        *int64 `json:"parentTaskId,omitempty"`
	Status          string `json:"status"`
	Direction       string `json:"direction"`
	Deleted         bool   `json:"deleted"`
	Disposition     string `json:"disposition"`
	ManifestID      string `json:"inventoryManifestId,omitempty"`
	LocalEntryCount int    `json:"localEntryCount"`
}

type Plan struct {
	Header               Header           `json:"source"`
	Integrity            Footer           `json:"integrity"`
	ReadyToImport        bool             `json:"readyToImport"`
	ConfigurationPresent bool             `json:"configurationPresent"`
	Categories           map[string]int64 `json:"categories"`
	Users                []UserPlan       `json:"users"`
	Connections          []ConnectionPlan `json:"connections"`
	Tasks                []TaskPlan       `json:"tasks"`
	ExpectedNodes        []string         `json:"expectedNodes"`
	Issues               []Issue          `json:"issues"`
	unknown              int64
	manifests            map[string]bool
	configuration        []byte
}

func invalid(message string) error { return errors.New("invalid legacy snapshot: " + message) }
func checksum(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && value == strings.ToLower(value)
}

// object rejects duplicate top-level fields, including duplicate type fields.
// It never interprets exported record values as executable Java/Fastjson types.
func object(line []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(line))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalid("expected one JSON object")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, invalid("invalid field")
		}
		name, ok := key.(string)
		if !ok {
			return nil, invalid("invalid field name")
		}
		if _, exists := fields[name]; exists {
			return nil, invalid("duplicate JSON field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, invalid("invalid field value")
		}
		fields[name] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, invalid("unfinished JSON object")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, invalid("trailing JSON data")
	}
	return fields, nil
}

func decode(line []byte, fields map[string]json.RawMessage, into any, names ...string) error {
	if len(fields) != len(names) {
		return invalid("unexpected or missing field")
	}
	for _, name := range names {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid("missing or null field")
		}
	}
	if err := json.Unmarshal(line, into); err != nil {
		return invalid("invalid field type")
	}
	return nil
}

func raw64(value string, maximum int) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(b) > maximum || base64.RawURLEncoding.EncodeToString(b) != value {
		return nil, invalid("invalid base64 or size bound")
	}
	return b, nil
}

// Inspect consumes the entire snapshot before returning a plan. A missing
// footer, truncation, edited entry, duplicate key or unsupported envelope fails
// closed. Callers must retain the original export for all raw legacy metadata.
func Inspect(ctx context.Context, in io.Reader) (Plan, error) {
	return inspectRecords(ctx, in, nil)
}

func inspectRecords(ctx context.Context, in io.Reader, visit func(key, value []byte) error) (Plan, error) {
	plan := Plan{Categories: map[string]int64{}, manifests: map[string]bool{}, Users: []UserPlan{}, Connections: []ConnectionPlan{}, Tasks: []TaskPlan{}, ExpectedNodes: []string{}, Issues: []Issue{}}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), MaxLineBytes)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, invalid("unterminated line")
		}
		return 0, nil, nil
	})
	digest := sha256.New()
	var previous []byte
	var entries, keyBytes, valueBytes int64
	header, footer, records := false, false, false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		line := scanner.Bytes()
		if footer {
			return Plan{}, invalid("data follows footer")
		}
		if !utf8.Valid(line) || bytes.ContainsRune(line, '\r') {
			return Plan{}, invalid("invalid line encoding")
		}
		fields, err := object(line)
		if err != nil {
			return Plan{}, err
		}
		var kind string
		if err := json.Unmarshal(fields["type"], &kind); err != nil {
			return Plan{}, invalid("missing record type")
		}
		if !header && kind != "header" {
			return Plan{}, invalid("header must be first")
		}
		switch kind {
		case "header":
			if header {
				return Plan{}, invalid("repeated header")
			}
			if err := decode(line, fields, &plan.Header, "type", "schemaVersion", "node", "role", "legacyJarSha256", "rocksdbJniSha256", "exportedAt"); err != nil {
				return Plan{}, err
			}
			if plan.Header.SchemaVersion != 1 || plan.Header.Node == "" || len(plan.Header.Node) > 1024 || strings.ContainsAny(plan.Header.Node, "\x00\r\n") || !checksum(plan.Header.LegacyJarSHA256) || !checksum(plan.Header.RocksDBJNISHA256) || plan.Header.ExportedAt.IsZero() {
				return Plan{}, invalid("unsupported or invalid header")
			}
			if role := plan.Header.Role; role != "master" && role != "agent" && role != "worker" {
				return Plan{}, invalid("unknown node role")
			}
			header = true
		case "configuration":
			if plan.ConfigurationPresent || records {
				return Plan{}, invalid("configuration must appear once before entries")
			}
			var row struct{ Type, Name, Value string }
			if err := decode(line, fields, &row, "type", "name", "value"); err != nil {
				return Plan{}, err
			}
			if row.Name != "application.yml" {
				return Plan{}, invalid("unknown configuration attachment")
			}
			plan.configuration, err = raw64(row.Value, 4<<20)
			if err != nil {
				return Plan{}, err
			}
			plan.ConfigurationPresent = true
		case "entry":
			records = true
			var row struct{ Type, Key, Value string }
			if err := decode(line, fields, &row, "type", "key", "value"); err != nil {
				return Plan{}, err
			}
			key, err := raw64(row.Key, 1<<20)
			if err != nil {
				return Plan{}, err
			}
			value, err := raw64(row.Value, 64<<20)
			if err != nil {
				return Plan{}, err
			}
			if entries > 0 && bytes.Compare(previous, key) >= 0 {
				return Plan{}, invalid("keys must be unique and bytewise ordered")
			}
			previous = append(previous[:0], key...)
			entries++
			keyBytes += int64(len(key))
			valueBytes += int64(len(value))
			if err := plan.observe(key, value); err != nil {
				return Plan{}, err
			}
			if visit != nil {
				if err := visit(key, value); err != nil {
					return Plan{}, err
				}
			}
		case "footer":
			if err := decode(line, fields, &plan.Integrity, "type", "entries", "keyBytes", "valueBytes", "sha256"); err != nil {
				return Plan{}, err
			}
			got := plan.Integrity
			if got.Entries != entries || got.KeyBytes != keyBytes || got.ValueBytes != valueBytes || !checksum(got.SHA256) || got.SHA256 != hex.EncodeToString(digest.Sum(nil)) {
				return Plan{}, invalid("footer checksum or counts mismatch")
			}
			footer = true
		default:
			return Plan{}, invalid("unknown envelope type")
		}
		if !footer {
			_, _ = digest.Write(line)
			_, _ = digest.Write([]byte{'\n'})
		}
	}
	if err := scanner.Err(); err != nil {
		return Plan{}, invalid("read failed or line exceeds size bound")
	}
	if !footer {
		return Plan{}, invalid("missing complete footer")
	}
	plan.finish()
	return plan, nil
}

func (p *Plan) issue(code, scope, message string) {
	p.Issues = append(p.Issues, Issue{code, scope, message})
}

func (p *Plan) finish() {
	p.issue("INSPECTION_ONLY", "", "Inspection has not created a runtime database. Run prepare, convert-tasks and prepare-recovery before finalize; keep every raw export until recovery is verified.")
	if !p.ConfigurationPresent {
		p.issue("CONFIGURATION_MISSING", "", "Capture the effective deployment settings; RocksDB does not contain all rate, URL and process defaults.")
	} else {
		p.issue("EFFECTIVE_CONFIG_REQUIRED", "", "application.yml is preserved verbatim. Resolve environment and command-line overrides separately; this tool does not evaluate YAML or shell expressions.")
	}
	if p.Header.Role == "master" {
		p.issue("SPLIT_MASTER_EXECUTION", "", "An old Master may also execute transfers. Its execution ledger requires a separate Go Agent database.")
	}
	if p.unknown > 0 {
		p.issue("UNKNOWN_KEYS_RETAINED", "", fmt.Sprintf("%d unknown keys remain in the raw snapshot; classify them before conversion.", p.unknown))
	}
	for _, t := range p.Tasks {
		if t.ManifestID != "" && !p.manifests[t.ManifestID] {
			p.issue("INVENTORY_NOT_READY", t.LegacyKey, "The task references an absent or unready inventory snapshot; never substitute a bucket scan.")
		}
	}
	if p.Categories["completion-ledger"] > 0 || p.Categories["file-report-ledger"] > 0 {
		p.issue("LEGACY_COMPLETION_NEEDS_RECONCILIATION", "", "Old completion records do not contain source version and destination identity. Preserve historical results; do not treat them as verified Go recovery records.")
	}
	if p.Categories["temporary-files"] > 0 {
		p.issue("OLD_TEMPORARY_FILES", "", "Preserve old ownership records. The Go process must not adopt or delete Java temporary files by filename.")
	}
	if p.Categories["task-bandwidth"] > 0 {
		p.issue("SHARED_BANDWIDTH_SCOPE", "", "Reconcile legacy bandwidth scopes and fixed members before translating them to Go parent allocations.")
	}
}
