package transferstore

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// These fixed signatures are published AWS test vectors, not expected values
// calculated by this implementation. Credentials are the documentation's fake
// examples. Source: AWS RESTAuthentication.html, Authentication examples.
func TestV2PublishedAWSVectors(t *testing.T) {
	const access = "AKIAIOSFODNN7EXAMPLE"
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	tests := []struct {
		name, method, url, bucket, date, contentType, signature string
		virtual                                                 bool
		headers                                                 http.Header
	}{
		{name: "object get", method: "GET", url: "https://awsexamplebucket1.s3.us-west-1.amazonaws.com/photos/puppy.jpg", bucket: "awsexamplebucket1", date: "Tue, 27 Mar 2007 19:36:42 +0000", signature: "qgk2+6Sv9/oM7G3qLEjTH1a1l1g=", virtual: true},
		{name: "object put", method: "PUT", url: "https://awsexamplebucket1.s3.us-west-1.amazonaws.com/photos/puppy.jpg", bucket: "awsexamplebucket1", date: "Tue, 27 Mar 2007 21:15:45 +0000", contentType: "image/jpeg", signature: "iqRzw+ileNPu1fhspnRs8nOjjIA=", virtual: true},
		{name: "list excludes parameters", method: "GET", url: "https://awsexamplebucket1.s3.us-west-1.amazonaws.com/?prefix=photos&max-keys=50&marker=puppy", bucket: "awsexamplebucket1", date: "Tue, 27 Mar 2007 19:42:41 +0000", signature: "m0WP8eCtspQl5Ahe6L1SozdX9YA=", virtual: true},
		{name: "acl subresource", method: "GET", url: "https://awsexamplebucket1.s3.us-west-1.amazonaws.com/?acl", bucket: "awsexamplebucket1", date: "Tue, 27 Mar 2007 19:44:46 +0000", signature: "82ZHiFIjc+WbcwFKGUVEQspPn+0=", virtual: true},
		{name: "unicode escaped path case", method: "GET", url: "https://s3.us-west-1.amazonaws.com/dictionary/fran%C3%A7ais/pr%c3%a9f%c3%a8re", date: "Wed, 28 Mar 2007 01:49:49 +0000", signature: "DNEZGsoieTZ92F3bUfSPQcbGmlM="},
		{name: "metadata multiple values", method: "PUT", url: "https://static.example.com:8080/db-backup.dat.gz", bucket: "static.example.com", date: "Tue, 27 Mar 2007 21:06:08 +0000", contentType: "application/x-download", signature: "jtBQa0Aq+DkULFI8qrpwIjGEx0E=", virtual: true, headers: http.Header{"Content-Md5": {"4gJE4saaMU4BqNR0kLY+lw=="}, "X-Amz-Acl": {"public-read"}, "X-Amz-Meta-Reviewedby": {"joe@example.com", "jane@example.com"}, "X-Amz-Meta-Filechecksum": {"0x02661779"}, "X-Amz-Meta-Checksumalgorithm": {"crc32"}, "Content-Encoding": {"gzip"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, test.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.headers != nil {
				request.Header = test.headers.Clone()
			}
			request.Header.Set("Date", test.date)
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			canonical, err := v2StringToSign(request, test.bucket, test.virtual)
			if err != nil {
				t.Fatal(err)
			}
			actual := v2Authorization(access, secret, canonical)
			if actual != "AWS "+access+":"+test.signature {
				t.Fatalf("published signature mismatch: %s\ncanonical=%q", actual, canonical)
			}
		})
	}
}

func TestV2MultipartResourceAndPathStyles(t *testing.T) {
	suffix := "/%E4%B8%AD%E6%96%87/a%20b+%25?uploadId=id%2B%2F%3D&partNumber=2&ignored=x"
	path, _ := http.NewRequest("PUT", "https://s3.example.com/bucket"+suffix, nil)
	virtual, _ := http.NewRequest("PUT", "https://bucket.s3.example.com"+suffix, nil)
	expected := "PUT\n\n\n\n/bucket/%E4%B8%AD%E6%96%87/a%20b+%25?partNumber=2&uploadId=id+/="
	for _, value := range []struct {
		r       *http.Request
		virtual bool
	}{{path, false}, {virtual, true}} {
		actual, err := v2StringToSign(value.r, "bucket", value.virtual)
		if err != nil || actual != expected {
			t.Fatalf("multipart canonicalization %q %v", actual, err)
		}
	}
	init, _ := http.NewRequest("POST", "https://s3.example.com/bucket/key?uploads=", nil)
	actual, err := v2StringToSign(init, "bucket", false)
	if err != nil || actual != "POST\n\n\n\n/bucket/key?uploads" {
		t.Fatalf("init canonicalization %q %v", actual, err)
	}
	response, _ := http.NewRequest("GET", "https://s3.example.com/bucket/key?response-content-disposition=attachment%3B%20filename%3Da%20b", nil)
	actual, err = v2StringToSign(response, "bucket", false)
	if err != nil || actual != "GET\n\n\n\n/bucket/key?response-content-disposition=attachment; filename=a b" {
		t.Fatalf("response override %q %v", actual, err)
	}
}

func TestV2SignerRefreshesDateAndIncludesSessionToken(t *testing.T) {
	signer := signatureV2{bucket: "bucket"}
	request, _ := http.NewRequest("GET", "https://bucket.s3.example.com/key", nil)
	request.Header.Set("X-Amz-Date", "stale-v4-date")
	request.Header.Set("X-Amz-Content-Sha256", "v4-only")
	credentials := aws.Credentials{AccessKeyID: "fake", SecretAccessKey: "fake-secret", SessionToken: "session-token"}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := signer.SignHTTP(context.Background(), credentials, request, "", "", "", now); err != nil {
		t.Fatal(err)
	}
	first := request.Header.Get("Authorization")
	if request.Header.Get("Date") != now.Format(http.TimeFormat) || request.Header.Get("X-Amz-Date") != "" || request.Header.Get("X-Amz-Content-Sha256") != "" || request.Header.Get("X-Amz-Security-Token") != "session-token" {
		t.Fatal("V2 headers retained V4 state or lost token")
	}
	canonical, err := v2StringToSign(request, "bucket", true)
	if err != nil || !strings.Contains(canonical, "x-amz-security-token:session-token\n/bucket/key") {
		t.Fatalf("unsigned security token %q %v", canonical, err)
	}
	if err = signer.SignHTTP(context.Background(), credentials, request, "", "", "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Authorization") == first || request.Header.Get("Date") == now.Format(http.TimeFormat) {
		t.Fatal("retry reused old signing time")
	}
}

func TestS3V2SDKMultipartRetryProtocol(t *testing.T) {
	p := newProtocolS3(t)
	p.failFirstPart = true
	store, err := NewS3(S3Config{Endpoint: p.server.URL, Region: "test-region", Bucket: "test-bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", SignatureVersion: "v2", PathStyle: true, PartSize: minPartSize, RetryMaxAttempts: 2, HTTPClient: p.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), int(minPartSize+1))
	result, err := store.Write(context.Background(), "v2/中文 a", bytes.NewReader(payload), WriteOptions{Size: int64(len(payload)), Overwrite: true})
	if err != nil || !result.Committed {
		t.Fatalf("V2 multipart %+v %v", result, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.v2Signed || p.partAttempts[1] != 2 || !bytes.Equal(p.objects["v2/中文 a"].data, payload) {
		t.Fatal("V2 SDK flow lost signing, retry, or data")
	}
}
