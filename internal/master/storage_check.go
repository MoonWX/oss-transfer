package master

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// storageProblem is one storage failure as users read it: a short catalog
// sentence (formatted with args) first, the redacted raw error as detail.
type storageProblem struct {
	kind          transferstore.StorageErrorKind
	message       string
	args          []any
	detail        string
	pathStyleHint bool
	// cause, when set, is summarized as the message's last argument.
	cause *storageProblem
}

// summary is the localized sentence, or the raw error when the failure is
// not one of the common kinds.
func (p *storageProblem) summary(w http.ResponseWriter) string {
	if p.message == "" {
		return p.detail
	}
	args := p.args
	if p.cause != nil {
		args = append(append([]any(nil), args...), p.cause.summary(w))
	}
	return fmt.Sprintf(localize(w, p.message), args...)
}

func (p *storageProblem) reasonCode() string {
	if p.kind == transferstore.StorageErrorUnknown {
		return "STORAGE_ERROR"
	}
	return "STORAGE_" + strings.ToUpper(string(p.kind))
}

// storageCheckOutcome separates what blocks saving a connection (failure)
// from what only deserves attention (warning).
type storageCheckOutcome struct {
	check   transferstore.ConnectionCheck
	failure *storageProblem
	warning *storageProblem
	// saveFailure blocks saving the connection, not listing its buckets:
	// an addressing failure found on the first listed bucket holds for
	// every bucket of the connection. warning reports it too.
	saveFailure *storageProblem
	pathStyle   bool
	// complete is false when the Agent predates bucket-level checks.
	complete bool
}

// newStorageProblem chooses the sentence for a classified failure. host is the
// endpoint host (with port); bucket names the bucket of a bucket-level
// request, empty for ListBuckets.
func newStorageProblem(kind transferstore.StorageErrorKind, detail string, config storageconfig.Config, bucket string, pathStyle bool) *storageProblem {
	problem := &storageProblem{kind: kind, detail: detail}
	endpoint, _ := url.Parse(config.Endpoint)
	host, hostname := config.Endpoint, config.Endpoint
	if endpoint != nil && endpoint.Host != "" {
		host, hostname = endpoint.Host, endpoint.Hostname()
	}
	addressingHint := bucket != "" && transferstore.StorageErrorSuggestsPathStyle(kind, pathStyle)
	switch kind {
	case transferstore.StorageErrorDNS:
		if addressingHint {
			problem.message, problem.args = "The bucket host name %s cannot be resolved. Self-hosted S3 (MinIO, SeaweedFS, Ceph) needs path-style addressing: set Addressing to Path-style.", []any{bucket + "." + hostname}
		} else {
			problem.message, problem.args = "The endpoint host name %s cannot be resolved. Check the endpoint address and the DNS settings of the execution node.", []any{hostname}
		}
	case transferstore.StorageErrorEOF:
		if addressingHint {
			problem.message = "The storage service closed the connection (EOF) when the bucket was addressed by host name. Self-hosted S3 (MinIO, SeaweedFS, Ceph) needs path-style addressing: set Addressing to Path-style."
		} else {
			problem.message, problem.args = "%s closed the connection (EOF). Check the endpoint address, port and protocol (http or https).", []any{host}
		}
	case transferstore.StorageErrorConnectionRefused:
		problem.message, problem.args = "%s refused the connection. Check the endpoint address and port, and that the storage service is running.", []any{host}
	case transferstore.StorageErrorConnectionReset:
		problem.message, problem.args = "The connection to %s was reset. Check the endpoint protocol (http or https), the port, and any proxy or firewall.", []any{host}
	case transferstore.StorageErrorTimeout:
		problem.message, problem.args = "The connection to %s timed out. Check the network and firewall between the execution node and the endpoint.", []any{host}
	case transferstore.StorageErrorTLS:
		problem.message, problem.args = "The TLS certificate of %s cannot be verified. Use a certificate the execution node trusts, or check whether the endpoint should use http.", []any{host}
	case transferstore.StorageErrorAccessDenied:
		problem.message = "Access denied: the credentials are valid but lack permission for this request."
	case transferstore.StorageErrorNoSuchBucket:
		if bucket != "" {
			problem.message, problem.args = "Bucket %s does not exist. Check the bucket name and the endpoint.", []any{bucket}
		} else {
			problem.message = "The bucket does not exist. Check the bucket name and the endpoint."
		}
	case transferstore.StorageErrorInvalidAccessKey:
		problem.message = "The Access Key does not exist. Check the Access Key."
	case transferstore.StorageErrorSignatureMismatch:
		problem.message = "Signature mismatch. Check the Secret Key and the signature version."
	}
	problem.pathStyleHint = addressingHint
	return problem
}

// checkStorage runs the connection check on one online Agent. An error means
// the check could not run (no Agent, Agent unavailable, invalid settings);
// storage failures are in the outcome.
func (s *Server) checkStorage(ctx context.Context, config storageconfig.Config, bucket string) (storageCheckOutcome, error) {
	outcome := storageCheckOutcome{pathStyle: config.EffectivePathStyle(), complete: true}
	members, err := s.onlineAgents()
	if err != nil {
		return outcome, err
	}
	if len(members) == 0 {
		return outcome, errors.New("no Agent is online; join an Agent before validating storage")
	}
	redact := func(message string) string {
		return redactStorageError(errors.New(message), []storageconfig.Config{config}).Error()
	}
	request := struct {
		Config storageconfig.Config `json:"config"`
		Bucket string               `json:"bucket,omitempty"`
	}{config, bucket}
	err = s.agentCall(ctx, members[0].Session(), "storage_check", false, request, &outcome.check)
	var agentError *AgentError
	if errors.As(err, &agentError) && agentError.Status == http.StatusNotFound && agentError.Message == "unknown_operation" {
		// Agents before v4.2.0 only list buckets.
		outcome.complete = false
		var listed struct {
			Buckets []string `json:"buckets"`
		}
		err = s.agentCall(ctx, members[0].Session(), "storage_buckets", false, config, &listed)
		outcome.check = transferstore.ConnectionCheck{Buckets: listed.Buckets, PathStyle: outcome.pathStyle}
		if errors.As(err, &agentError) {
			outcome.check.ListError, outcome.check.ListErrorKind = agentError.Message, transferstore.ClassifyStorageMessage(agentError.Message)
			err = nil
		}
	}
	if err != nil {
		return outcome, redactStorageError(err, []storageconfig.Config{config})
	}
	if outcome.check.Buckets == nil {
		outcome.check.Buckets = []string{}
	}
	outcome.pathStyle = outcome.check.PathStyle
	check := outcome.check
	if check.ListError != "" {
		problem := newStorageProblem(check.ListErrorKind, redact(check.ListError), config, "", outcome.pathStyle)
		if check.ListErrorKind == transferstore.StorageErrorAccessDenied {
			// The key and signature were accepted: bucket-scoped keys.
			problem.message, problem.args = "These credentials cannot list buckets (ListBuckets was denied), so only a bucket-level check proves the connection. Keys limited to one bucket still work: enter the bucket name when you create a task.", nil
			if check.Bucket == "" {
				outcome.warning = problem
			}
		} else {
			outcome.failure = problem
			return outcome, nil
		}
	}
	if check.BucketError != "" {
		problem := newStorageProblem(check.BucketErrorKind, redact(check.BucketError), config, check.Bucket, outcome.pathStyle)
		switch {
		case check.BucketErrorKind == transferstore.StorageErrorAccessDenied:
			problem.message, problem.args = "The credentials cannot list the objects of bucket %s (AccessDenied). Check the bucket permissions before you migrate from this bucket.", []any{check.Bucket}
			outcome.warning = problem
		case bucket == "" && check.ListError == "":
			// Nobody named this bucket: it is the first listed one, and
			// other buckets can still work (one in another region, for
			// example). The buckets were listed, so the connection works.
			outcome.warning = &storageProblem{kind: problem.kind, message: "The connection works, but the check of bucket %s, the first listed bucket, failed: %s", args: []any{check.Bucket}, cause: problem, detail: problem.detail, pathStyleHint: problem.pathStyleHint}
			if problem.pathStyleHint {
				outcome.saveFailure = problem
			}
		default:
			outcome.failure = problem
		}
	}
	return outcome, nil
}

// storageFailureResponse reports a failed check: the localized sentence as the
// message, the raw error as detail.
func storageFailureResponse(w http.ResponseWriter, status int, prefix string, problem *storageProblem) {
	writeJSON(w, status, map[string]any{
		"success": false, "message": localize(w, prefix) + problem.summary(w),
		"detail": problem.detail, "reasonCode": problem.reasonCode(), "pathStyleHint": problem.pathStyleHint,
	})
}

// storageWarningFields adds a non-blocking warning to a response.
func storageWarningFields(w http.ResponseWriter, fields map[string]any, problem *storageProblem) {
	if problem == nil {
		return
	}
	fields["warning"] = problem.summary(w)
	fields["warningDetail"] = problem.detail
	fields["warningCode"] = problem.reasonCode()
	if problem.pathStyleHint {
		fields["warningPathStyleHint"] = true
	}
}
