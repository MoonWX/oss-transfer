package transferstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var ErrUploadIdentity = errors.New("multipart upload connection identity changed; cleanup record retained")

// S3ConnectionIdentity contains no credentials. The fingerprint detects even a
// credential rotation, so recovery never sends an old upload ID to a new account.
type S3ConnectionIdentity struct {
	Alias                 string `json:"alias"`
	Endpoint              string `json:"endpoint"`
	Region                string `json:"region"`
	PathStyle             bool   `json:"pathStyle"`
	SignatureVersion      string `json:"signatureVersion"`
	CredentialFingerprint string `json:"credentialFingerprint"`
}

type S3UploadRecord struct {
	Owner      string               `json:"owner"`
	ID         string               `json:"id"`
	Connection S3ConnectionIdentity `json:"connection"`
	Bucket     string               `json:"bucket"`
	Key        string               `json:"key"`
	UploadID   string               `json:"uploadId"`
}

type S3UploadRegistry interface {
	OwnerID() string
	RegisterUpload(context.Context, S3UploadRecord) error
	ForgetUpload(context.Context, string) error
}

func s3ConnectionIdentity(config S3Config) S3ConnectionIdentity {
	credentials, _ := json.Marshal([]string{config.AccessKey, config.SecretKey, config.SessionToken})
	fingerprint := sha256.Sum256(credentials)
	signature := strings.ToLower(config.SignatureVersion)
	if signature == "" {
		signature = "v4"
	}
	return S3ConnectionIdentity{Endpoint: config.Endpoint, Region: config.Region, PathStyle: config.PathStyle,
		SignatureVersion: signature, CredentialFingerprint: hex.EncodeToString(fingerprint[:])}
}

func (s *S3) ConnectionIdentity(alias string) S3ConnectionIdentity {
	identity := s.connection
	identity.Alias = alias
	return identity
}

func ValidateS3UploadRecord(record S3UploadRecord) error {
	if !validHex(record.Owner, 32) || !validHex(record.ID, 16) || record.Connection.Alias == "" ||
		!validHex(record.Connection.CredentialFingerprint, 32) || record.Bucket == "" || strings.ContainsAny(record.Bucket, "/\\\x00") ||
		record.Key == "" || len(record.Key) > 1024 || !utf8.ValidString(record.Key) || strings.ContainsRune(record.Key, 0) ||
		record.UploadID == "" || len(record.UploadID) > 8192 || !utf8.ValidString(record.UploadID) || strings.ContainsRune(record.UploadID, 0) {
		return ErrUploadIdentity
	}
	return nil
}

// SetRecoveryRegistry must run before concurrent use. The same Agent database
// owns remote upload records and any local retry spool belonging to those uploads.
func (s *S3) SetRecoveryRegistry(registry S3UploadRegistry, alias string, temporary LocalTempRegistry) error {
	if registry == nil || temporary == nil || alias == "" || !validHex(registry.OwnerID(), 32) || registry.OwnerID() != temporary.OwnerID() {
		return ErrUploadIdentity
	}
	s.uploadRegistry, s.tempRegistry, s.alias = registry, temporary, alias
	return nil
}

func noSuchUpload(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "NoSuchUpload"
}

// AbortRecordedUpload operates on one durably recorded upload only. Listing all
// multipart uploads and deleting by a key/prefix would include other clients.
func (s *S3) AbortRecordedUpload(ctx context.Context, record S3UploadRecord) error {
	if err := ValidateS3UploadRecord(record); err != nil {
		return err
	}
	if record.Bucket != s.bucket || record.Connection != s.ConnectionIdentity(record.Connection.Alias) {
		return ErrUploadIdentity
	}
	for attempt := 0; attempt < 3; attempt++ {
		_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(record.Key), UploadId: aws.String(record.UploadID)})
		if noSuchUpload(err) {
			return nil
		}
		if err != nil {
			return s3Error(err)
		}
		// AWS documents that in-flight parts can survive an initial Abort.
		// https://docs.aws.amazon.com/AmazonS3/latest/API/API_AbortMultipartUpload.html
		parts, err := s.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(s.bucket), Key: aws.String(record.Key), UploadId: aws.String(record.UploadID), MaxParts: aws.Int32(1)})
		if noSuchUpload(err) || (err == nil && len(parts.Parts) == 0 && !aws.ToBool(parts.IsTruncated)) {
			return nil
		}
		if err != nil {
			return s3Error(err)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return fmt.Errorf("multipart parts still present after abort; cleanup record retained")
}
