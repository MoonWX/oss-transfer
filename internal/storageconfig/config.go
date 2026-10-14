// Package storageconfig stores S3 connection settings and synchronizes complete,
// versioned snapshots. Saving a configuration does not validate network access.
package storageconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalid          = errors.New("invalid storage configuration")
	ErrExists           = errors.New("storage connection already exists")
	ErrNotFound         = errors.New("storage connection not found")
	ErrStaleRevision    = errors.New("storage snapshot is older than the applied revision")
	ErrRevisionConflict = errors.New("storage snapshot revision has different contents")
	ErrRevisionLimit    = errors.New("storage configuration revision limit reached")
)

const (
	maxConnections   = 1024
	maxSnapshotBytes = 4 << 20
)

type Config struct {
	Alias           string `json:"alias"`
	Endpoint        string `json:"endpoint"`
	AccessKey       string `json:"accessKey"`
	SecretKey       string `json:"secretKey"`
	UseV2SignerType *bool  `json:"useV2SignerType"`
	PathStyleEnable *bool  `json:"pathStyleEnable"`
	Region          string `json:"region,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`
}

// Snapshot contains credentials and is intended only for authenticated Agent
// synchronization, not a public status response or a log message.
type Snapshot struct {
	Revision    uint64   `json:"revision"`
	Connections []Config `json:"connections"`
}

func validString(value string, maximum int, required bool) bool {
	if len(value) > maximum || (required && value == "") || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

func validAlias(alias string) bool {
	if !validString(alias, 128, true) || alias == "." || alias == ".." {
		return false
	}
	for _, char := range alias {
		if !(unicode.IsLetter(char) || unicode.IsDigit(char) || char == '_' || char == '-' || char == '.') {
			return false
		}
	}
	return true
}

func invalid(field string) error { return fmt.Errorf("%w: %s", ErrInvalid, field) }

// Validate rejects malformed settings without rewriting values. In particular,
// leading or trailing spaces in credential values remain part of the credential.
func Validate(config Config) error {
	if !validAlias(config.Alias) {
		return invalid("alias must contain letters, digits, dots, underscores or hyphens")
	}
	if !validString(config.Endpoint, 2048, true) || config.Endpoint != strings.TrimSpace(config.Endpoint) {
		return invalid("endpoint")
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(config.Endpoint, "#") ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") || strings.HasSuffix(u.Host, ":") {
		return invalid("endpoint must be an HTTP or HTTPS origin")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return invalid("endpoint port")
		}
	}
	if !validString(config.AccessKey, 4096, true) {
		return invalid("accessKey")
	}
	if !validString(config.SecretKey, 4096, true) {
		return invalid("secretKey")
	}
	if !validString(config.SessionToken, 16384, false) {
		return invalid("sessionToken")
	}
	if !validString(config.Region, 128, false) || config.Region != strings.TrimSpace(config.Region) || strings.ContainsAny(config.Region, " /\\") {
		return invalid("region")
	}
	return nil
}

func cloneConfig(config Config) Config {
	if config.UseV2SignerType != nil {
		value := *config.UseV2SignerType
		config.UseV2SignerType = &value
	}
	if config.PathStyleEnable != nil {
		value := *config.PathStyleEnable
		config.PathStyleEnable = &value
	}
	return config
}

func canonical(snapshot Snapshot) (Snapshot, error) {
	if len(snapshot.Connections) > maxConnections || (snapshot.Revision == 0 && len(snapshot.Connections) != 0) {
		return Snapshot{}, invalid("snapshot size or revision")
	}
	result := Snapshot{Revision: snapshot.Revision, Connections: make([]Config, len(snapshot.Connections))}
	for index, config := range snapshot.Connections {
		if err := Validate(config); err != nil {
			return Snapshot{}, err
		}
		result.Connections[index] = cloneConfig(config)
	}
	sort.Slice(result.Connections, func(i, j int) bool { return result.Connections[i].Alias < result.Connections[j].Alias })
	for index := 1; index < len(result.Connections); index++ {
		if result.Connections[index-1].Alias == result.Connections[index].Alias {
			return Snapshot{}, invalid("duplicate alias")
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxSnapshotBytes {
		return Snapshot{}, invalid("snapshot exceeds size limit")
	}
	return result, nil
}
