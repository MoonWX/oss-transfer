package agentservice

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// SharedProofProtocol is advertised by Agents that implement
// shared_fingerprint_sample, shared_fingerprint_verify and shared_proof_skip.
const SharedProofProtocol = 1

// SharedFingerprintMismatchCode marks a verification whose sampled files this
// Agent sees differently; the response lists every difference.
const SharedFingerprintMismatchCode = "shared_fingerprint_mismatch"

// UnwritableStorageCode tells the Master that a shared probe could not be
// written only because this Agent cannot write the root: no permission, a
// read-only file system, or no space left.
const UnwritableStorageCode = "storage_not_writable"

// SharedFingerprintSampleRequest asks one Agent to fingerprint up to Limit
// existing files under Root (at most transferstore.SharedFingerprintMaxFiles;
// zero means that maximum). Portable restricts the sample to names that a
// Windows Agent can open; the Master sets it when one takes part.
type SharedFingerprintSampleRequest struct {
	Root     string `json:"root"`
	Limit    int    `json:"limit,omitempty"`
	Portable bool   `json:"portable,omitempty"`
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

// SharedFingerprintMismatch is the body of a 409 shared_fingerprint_mismatch.
type SharedFingerprintMismatch struct {
	Code        string                                      `json:"code"`
	Message     string                                      `json:"message"`
	Differences []transferstore.SharedFingerprintDifference `json:"differences"`
}

// SharedProofSkipRequest records a proof for a task whose operator chose to
// skip the shared-directory check; see SharedVerifier.Skip.
type SharedProofSkipRequest struct {
	ParentID   int64  `json:"parentId"`
	Generation uint64 `json:"generation"`
	Root       string `json:"root"`
}

// SharedProofSkipResponse says whether this Agent process held no proof for
// the task generation and root before, so the Master logs each new grant.
type SharedProofSkipResponse struct {
	Verified bool `json:"verified"`
	Granted  bool `json:"granted"`
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

// sharedWriteResponse marks a probe write refused because this Agent cannot
// write the root, so the Master can compare sampled files of an upload source
// instead of failing.
func sharedWriteResponse(value any, err error) (agenttransport.Response, error) {
	if err != nil && transferstore.IsUnwritableStorage(err) {
		data, _ := json.Marshal(map[string]string{"code": UnwritableStorageCode, "message": err.Error()})
		return agenttransport.Response{Status: 403, Payload: data}, nil
	}
	return response(value, err)
}

func sharedFingerprintResponse(value any, err error) (agenttransport.Response, error) {
	var mismatch *transferstore.SharedFingerprintMismatchError
	if errors.As(err, &mismatch) {
		data, _ := json.Marshal(SharedFingerprintMismatch{Code: SharedFingerprintMismatchCode, Message: err.Error(), Differences: mismatch.Differences})
		return agenttransport.Response{Status: 409, Payload: data}, nil
	}
	return response(value, err)
}
