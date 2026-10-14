package transferstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// S3 V2 canonicalization follows the AWS REST authentication specification:
// https://docs.aws.amazon.com/AmazonS3/latest/developerguide/RESTAuthentication.html
// This plugs into the SDK signing stage, which runs for each retry. Request paths
// and bodies remain SDK-owned; only authentication headers change.
type signatureV2 struct {
	bucket         string
	forcePathStyle bool
	endpointHost   string
}

func (s signatureV2) SignHTTP(ctx context.Context, credentials aws.Credentials, request *http.Request, _ string, _ string, _ string, now time.Time, _ ...func(*v4.SignerOptions)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request.Header.Del("Authorization")
	request.Header.Del("X-Amz-Content-Sha256")
	request.Header.Del("X-Amz-Date")
	request.Header.Set("Date", now.UTC().Format(http.TimeFormat))
	if credentials.SessionToken != "" {
		request.Header.Set("X-Amz-Security-Token", credentials.SessionToken)
	} else {
		request.Header.Del("X-Amz-Security-Token")
	}
	host := request.URL.Hostname()
	if request.Host != "" {
		if parsed, err := url.Parse("http://" + request.Host); err == nil {
			host = parsed.Hostname()
		}
	}
	virtual := !s.forcePathStyle && !strings.EqualFold(host, s.endpointHost) && strings.HasPrefix(strings.ToLower(host), strings.ToLower(s.bucket)+".")
	canonical, err := v2StringToSign(request, s.bucket, virtual)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", v2Authorization(credentials.AccessKeyID, credentials.SecretAccessKey, canonical))
	return nil
}

func v2Authorization(accessKey, secret, canonical string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return "AWS " + accessKey + ":" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

var foldedHeader = regexp.MustCompile(`\r?\n[\t ]+`)
var v2Subresources = map[string]bool{
	"acl": true, "accelerate": true, "analytics": true, "cors": true, "delete": true, "inventory": true, "legal-hold": true, "lifecycle": true, "location": true, "logging": true, "metrics": true, "notification": true, "object-lock": true, "partNumber": true, "policy": true, "replication": true, "requestPayment": true, "restore": true, "retention": true, "tagging": true, "torrent": true, "uploadId": true, "uploads": true, "versionId": true, "versioning": true, "versions": true, "website": true,
	"response-content-type": true, "response-content-language": true, "response-expires": true, "response-cache-control": true, "response-content-disposition": true, "response-content-encoding": true,
}

func v2StringToSign(request *http.Request, bucket string, virtual bool) (string, error) {
	canonicalHeaders := map[string][]string{}
	names := make([]string, 0, len(request.Header))
	for name := range request.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		for _, value := range request.Header[name] {
			canonicalHeaders[lower] = append(canonicalHeaders[lower], strings.TrimSpace(foldedHeader.ReplaceAllString(value, " ")))
		}
	}
	sorted := make([]string, 0, len(canonicalHeaders))
	for name := range canonicalHeaders {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var amz strings.Builder
	for _, name := range sorted {
		amz.WriteString(name)
		amz.WriteByte(':')
		amz.WriteString(strings.Join(canonicalHeaders[name], ","))
		amz.WriteByte('\n')
	}
	resource := request.URL.EscapedPath()
	if resource == "" {
		resource = "/"
	}
	if virtual {
		resource = "/" + bucket + resource
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return "", errors.New("invalid S3 query encoding")
	}
	parameters := make([]string, 0, len(query))
	for name, values := range query {
		if !v2Subresources[name] {
			continue
		}
		if len(values) != 1 {
			return "", errors.New("duplicate S3 signature subresource")
		}
		value := name
		if values[0] != "" {
			value += "=" + values[0]
		}
		parameters = append(parameters, value)
	}
	sort.Strings(parameters)
	if len(parameters) > 0 {
		resource += "?" + strings.Join(parameters, "&")
	}
	date := request.Header.Get("Date")
	if len(canonicalHeaders["x-amz-date"]) > 0 {
		date = ""
	}
	return request.Method + "\n" + request.Header.Get("Content-MD5") + "\n" + request.Header.Get("Content-Type") + "\n" + date + "\n" + amz.String() + resource, nil
}
