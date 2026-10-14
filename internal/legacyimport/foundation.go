package legacyimport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agent"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

type importedUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash,omitempty"`
	DisplayName  string `json:"displayName"`
	Role         string `json:"role"`
	Revision     uint64 `json:"revision"`
}
type oldMember struct {
	InstanceID, EnrollmentID, Address, Label, CredentialHash string
	Enabled                                                  *bool
	JoinedAt                                                 int64
}
type oldIdentity struct {
	MasterURL, Address, Credential, InstanceID string
	Joined                                     bool
}
type oldNodeConfig struct {
	LocalIP              string            `json:"localIp"`
	WorkerIPs            []string          `json:"workerIps"`
	AdvertisedLocalRoots []string          `json:"advertisedLocalRoots"`
	NodeLabels           map[string]string `json:"nodeLabels"`
}
type foundationRecords struct {
	users       []importedUser
	connections []storageconfig.Config
	members     map[string]oldMember
	identity    *oldIdentity
	node        oldNodeConfig
	overrides   map[string]float64
}

func (p *Prepared) foundationRecords(ctx context.Context, node string) (foundationRecords, error) {
	data := foundationRecords{members: map[string]oldMember{}, overrides: map[string]float64{}}
	err := p.Iterate(ctx, node, func(key, value []byte) error {
		k := string(key)
		switch {
		case strings.HasPrefix(k, "user:"):
			var u importedUser
			if json.Unmarshal(value, &u) != nil || !safeIdentity(u.Username, 128) || u.Username != strings.TrimSpace(u.Username) || u.Username == "" {
				return invalid("invalid imported username")
			}
			u.Revision = 1
			data.users = append(data.users, u)
		case strings.HasPrefix(k, "s3_config_"):
			var config storageconfig.Config
			if json.Unmarshal(value, &config) != nil {
				return invalid("malformed connection")
			}
			config.Region = "default"
			if err := storageconfig.Validate(config); err != nil {
				return invalid("connection is not compatible with Go configuration: " + config.Alias)
			}
			data.connections = append(data.connections, config)
		case k == "node_config":
			if json.Unmarshal(value, &data.node) != nil {
				return invalid("malformed node configuration")
			}
		case k == "agent:identity":
			var identity oldIdentity
			if json.Unmarshal(value, &identity) != nil {
				return invalid("malformed Agent identity")
			}
			data.identity = &identity
		case strings.HasPrefix(k, "agent:member:"):
			var member oldMember
			if json.Unmarshal(value, &member) != nil || member.Address != strings.TrimPrefix(k, "agent:member:") {
				return invalid("malformed Agent membership")
			}
			data.members[member.Address] = member
		case strings.HasPrefix(k, "rate_limit:manual:"):
			kind := strings.TrimPrefix(k, "rate_limit:manual:")
			var valueRate float64
			if kind != "list" && kind != "put" && kind != "head" && kind != "bandwidth" {
				return invalid("unknown manual rate kind")
			}
			if json.Unmarshal(value, &valueRate) != nil || !compatibleRate(kind, valueRate) {
				return invalid("invalid manual rate")
			}
			data.overrides[kind] = valueRate
		}
		return nil
	})
	return data, err
}

func compatibleRate(kind string, value float64) bool {
	if math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
		return false
	}
	if kind == "bandwidth" {
		return value <= 1_000_000_000_000 && (value == 0 || value >= 8)
	}
	return value <= 1_000_000 && (value == 0 || value >= 0.01)
}

func safeIdentity(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}
func safeInstance(value string) bool {
	if !safeIdentity(value, 128) || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
func validCredential(value string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func (p *Prepared) prepareFoundation(ctx context.Context, options PrepareOptions) error {
	records := map[string]foundationRecords{}
	masterNode := ""
	for _, source := range p.Manifest.Sources {
		data, err := p.foundationRecords(ctx, source.Node)
		if err != nil {
			return err
		}
		records[source.Node] = data
		if source.Role == "master" {
			masterNode = source.Node
		}
	}
	master := records[masterNode]
	admin := false
	for _, user := range master.users {
		admin = admin || user.Role == "admin"
	}
	if !admin {
		return invalid("Master snapshot must contain an existing administrator; no default account will be generated")
	}
	encodedConnections, err := json.Marshal(master.connections)
	if err != nil || len(encodedConnections) > 4<<20 || len(master.connections) > 1024 {
		return invalid("too many connections for the Go runtime")
	}
	snapshot := storageconfig.Snapshot{Revision: 1, Connections: master.connections}
	sort.Slice(snapshot.Connections, func(i, j int) bool { return snapshot.Connections[i].Alias < snapshot.Connections[j].Alias })
	members := map[string]cluster.Member{}
	credentials := map[string]string{}
	addresses := map[string]string{}
	identityByNode := map[string]agent.Identity{}
	for address, old := range master.members {
		if !safeInstance(old.InstanceID) || !safeIdentity(old.EnrollmentID, 128) || !safeIdentity(address, 256) || len(old.Label) > 128 || strings.ContainsAny(old.Label, "\x00\r\n") {
			return invalid("unsupported Agent member identity")
		}
		hashBytes, err := base64.RawURLEncoding.Strict().DecodeString(old.CredentialHash)
		if err != nil || len(hashBytes) != sha256.Size || base64.RawURLEncoding.EncodeToString(hashBytes) != old.CredentialHash || old.Enabled == nil {
			return invalid("unsupported V4.0.x Agent credential hash")
		}
		member := cluster.Member{ID: old.InstanceID, EnrollmentID: old.EnrollmentID, Address: address, Label: old.Label, CredentialHash: hex.EncodeToString(hashBytes), Revoked: !*old.Enabled, JoinedAt: time.UnixMilli(old.JoinedAt).UTC()}
		if _, exists := members[member.ID]; exists {
			return invalid("duplicate Agent instance ID")
		}
		members[member.ID] = member
		if member.Revoked {
			// Retain revoked membership even when that retired host no longer has
			// an export. Never create active address or credential indexes for it.
			continue
		}
		data, found := records[address]
		if !found || p.plans[address].Header.Role != "agent" || data.identity == nil {
			return invalid("active Agent member has no matching private snapshot")
		}
		private := data.identity
		_, err = cleanOrigin(private.MasterURL)
		if err != nil || !private.Joined || private.InstanceID != old.InstanceID || private.Address != address || !validCredential(private.Credential) {
			return invalid("Agent private identity does not match Master membership")
		}
		h := sha256.Sum256([]byte(private.Credential))
		if base64.RawURLEncoding.EncodeToString(h[:]) != old.CredentialHash {
			return invalid("Agent credential does not match V4.0.x Master membership")
		}
		if _, exists := credentials[member.CredentialHash]; exists {
			return invalid("duplicate Agent credential")
		}
		credentials[member.CredentialHash], addresses[address] = member.ID, member.ID
		identityByNode[address] = agent.Identity{InstanceID: member.ID, EnrollmentID: member.EnrollmentID, MasterURL: p.Manifest.PublicURL, Credential: private.Credential, Address: member.Address, Label: member.Label}
	}
	for node, data := range records {
		role := p.plans[node].Header.Role
		if role == "agent" {
			if _, found := identityByNode[node]; found {
				continue
			}
			old, found := master.members[node]
			if !found || old.Enabled == nil || *old.Enabled || data.identity == nil || old.InstanceID != data.identity.InstanceID || !data.identity.Joined || !validCredential(data.identity.Credential) {
				return invalid("Agent snapshot has no matching Master membership")
			}
			h := sha256.Sum256([]byte(data.identity.Credential))
			if base64.RawURLEncoding.EncodeToString(h[:]) != old.CredentialHash {
				return invalid("revoked Agent identity does not match retained membership")
			}
			identityByNode[node] = agent.Identity{InstanceID: old.InstanceID, EnrollmentID: old.EnrollmentID, MasterURL: p.Manifest.PublicURL, Credential: data.identity.Credential, Address: node, Label: old.Label}
			continue
		}
		approval, found := options.DirectNodes[node]
		if !found {
			return invalid("direct worker requires explicit new Agent identity approval: " + node)
		}
		address := approval.Address
		if address == "" {
			address = node
		}
		if !safeIdentity(address, 256) || len(approval.Label) > 128 || strings.ContainsAny(approval.Label, "\x00\r\n") {
			return invalid("invalid direct worker mapping")
		}
		if _, exists := addresses[address]; exists {
			return invalid("duplicate mapped node address")
		}
		id, err := cluster.Token()
		if err != nil {
			return err
		}
		enrollment, err := cluster.Token()
		if err != nil {
			return err
		}
		credential, err := cluster.Token()
		if err != nil {
			return err
		}
		h := sha256.Sum256([]byte(credential))
		member := cluster.Member{ID: id, EnrollmentID: enrollment, Address: address, Label: approval.Label, CredentialHash: hex.EncodeToString(h[:]), JoinedAt: p.Manifest.CreatedAt}
		members[id], addresses[address], credentials[member.CredentialHash] = member, id, id
		identityByNode[node] = agent.Identity{InstanceID: id, EnrollmentID: enrollment, Credential: credential, MasterURL: p.Manifest.PublicURL, Address: address, Label: member.Label}
	}
	for node := range options.DirectNodes {
		if _, found := records[node]; !found || p.plans[node].Header.Role == "agent" {
			return invalid("direct node approval refers to an unknown or already enrolled Agent")
		}
	}
	p.Master, err = state.Open(filepath.Join(p.Directory, "master", "master.db"))
	if err != nil {
		return err
	}
	err = p.Master.Update(func(tx *state.Tx) error {
		if err := p.writeFence(tx, "master"); err != nil {
			return err
		}
		for _, user := range master.users {
			if err := tx.Put("users", user.Username, user); err != nil {
				return err
			}
		}
		configuration := struct {
			Configured bool      `json:"configured"`
			PublicURL  string    `json:"publicUrl"`
			CreatedAt  time.Time `json:"createdAt"`
		}{true, p.Manifest.PublicURL, p.Manifest.CreatedAt}
		if err := tx.Put("system", "configuration", configuration); err != nil {
			return err
		}
		if err := tx.Put("storage-configuration", "master", snapshot); err != nil {
			return err
		}
		for id, member := range members {
			if err := tx.Put("agent-members", id, member); err != nil {
				return err
			}
		}
		for hash, id := range credentials {
			if err := tx.Put("agent-credentials", hash, id); err != nil {
				return err
			}
		}
		for address, id := range addresses {
			if err := tx.Put("agent-addresses", address, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for node, identity := range identityByNode {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative := filepath.Join("agents", identity.InstanceID, "agent.db")
		db, err := state.Open(filepath.Join(p.Directory, relative))
		if err != nil {
			return err
		}
		p.Agents[node] = db
		member := members[identity.InstanceID]
		data := records[node]
		err = db.Update(func(tx *state.Tx) error {
			if err := p.writeFence(tx, "agent"); err != nil {
				return err
			}
			if err := tx.Put("agent-identity", "self", identity); err != nil {
				return err
			}
			if err := tx.Put("storage-configuration", "agent", snapshot); err != nil {
				return err
			}
			settings := map[string]any{"overrides": data.overrides, "effective": map[string]float64{}, "last": map[string]any{}, "incidents": map[string]string{}, "advertisedLocalRoots": data.node.AdvertisedLocalRoots, "sequence": 0}
			return tx.Put("node-control", "settings", settings)
		})
		if err != nil {
			return err
		}
		p.Manifest.Nodes = append(p.Manifest.Nodes, NodeIdentity{LegacyNode: node, InstanceID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address, Label: member.Label, Revoked: member.Revoked, Generated: p.plans[node].Header.Role != "agent", Database: filepath.ToSlash(relative)})
		if err := syncDirectory(filepath.Dir(filepath.Join(p.Directory, relative))); err != nil {
			return err
		}
	}
	p.Manifest.Accounts, p.Manifest.Connections = len(master.users), len(snapshot.Connections)
	for _, name := range []string{"master", "agents", "raw"} {
		if err := syncDirectory(filepath.Join(p.Directory, name)); err != nil {
			return err
		}
	}
	return nil
}

func (p *Prepared) writeFence(tx *state.Tx, role string) error {
	if err := tx.Put("metadata", "format", map[string]any{"role": role, "schemaVersion": 1}); err != nil {
		return err
	}
	return tx.Put("migration", "state", map[string]any{"pending": true, "migrationId": p.Manifest.MigrationID})
}
