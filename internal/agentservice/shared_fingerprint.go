package agentservice

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// SharedContentChangedCode marks a fingerprint mismatch where the sampled file
// exists but its size or content changed since sampling.
const SharedContentChangedCode = "shared_content_changed"

// ReadOnlyStorageCode tells the Master that a shared probe could not be
// written only because the root is read-only or not writable by this Agent.
const ReadOnlyStorageCode = "read_only_storage"

// SharedFingerprintSampleRequest asks one Agent to fingerprint up to Limit
// existing files under Root (at most transferstore.SharedFingerprintMaxFiles;
// zero means that maximum).
type SharedFingerprintSampleRequest struct {
	Root  string `json:"root"`
	Limit int    `json:"limit,omitempty"`
}

type SharedFingerprintSampleResponse struct {
	Fingerprints []transferstore.SharedFingerprint `json:"fingerprints"`
}

// SharedFingerprintVerifyRequest asks an Agent to recompute Fingerprints under
// Root. With ParentID and Generation it also records the Agent's proof for
// that task generation, like shared_probe_verify; without them it only checks.
type SharedFingerprintVerifyRequest struct {
	ParentID     int64                             `json:"parentId,omitempty"`
	Generation   uint64                            `json:"generation,omitempty"`
	Root         string                            `json:"root"`
	Fingerprints []transferstore.SharedFingerprint `json:"fingerprints"`
}

func (r SharedFingerprintSampleRequest) validate() error {
	if !filepath.IsAbs(r.Root) {
		return errors.New("shared fingerprint sampling requires an absolute directory")
	}
	if r.Limit < 0 || r.Limit > transferstore.SharedFingerprintMaxFiles {
		return errors.New("shared fingerprint sample limit is out of range")
	}
	return nil
}

func (r SharedFingerprintVerifyRequest) validate() error {
	if !filepath.IsAbs(r.Root) {
		return errors.New("shared fingerprint verification requires an absolute directory")
	}
	if r.ParentID < 0 || (r.ParentID == 0) != (r.Generation == 0) {
		return errors.New("shared fingerprint verification requires both parent and generation, or neither")
	}
	return transferstore.ValidateSharedFingerprints(r.Fingerprints)
}

// sharedWriteResponse marks a probe write refused by permissions or a
// read-only file system, so the Master can verify a read-only upload source
// by content fingerprints instead of failing.
func sharedWriteResponse(value any, err error) (agenttransport.Response, error) {
	if err != nil && transferstore.IsReadOnlyStorage(err) {
		data, _ := json.Marshal(map[string]string{"code": ReadOnlyStorageCode, "message": err.Error()})
		return agenttransport.Response{Status: 403, Payload: data}, nil
	}
	return response(value, err)
}

func sharedFingerprintResponse(value any, err error) (agenttransport.Response, error) {
	if errors.Is(err, transferstore.ErrSharedFingerprintChanged) {
		data, _ := json.Marshal(map[string]string{"code": SharedContentChangedCode, "message": err.Error()})
		return agenttransport.Response{Status: 409, Payload: data}, nil
	}
	return response(value, err)
}
