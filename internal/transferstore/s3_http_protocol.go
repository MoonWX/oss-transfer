package transferstore

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"strings"
)

// S3CompatibleTransport preserves the normal pool and gives JDCloud OSS a
// separate HTTP/1.1 pool. HEAD followed by conditional PUT on its HTTP/2
// gateway can return an HTML 400, even though the same requests work on HTTP/1.1.
// Install this below request admission/observation wrappers, not around them.
func S3CompatibleTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	pool, ok := base.(*http.Transport)
	if !ok {
		// An opaque wrapper may enforce limits or custom routing. Never unwrap
		// or replace it; its owner must configure the underlying pool.
		return base
	}
	http1 := pool.Clone()
	configureS3HTTP1(http1)
	return &s3ProtocolTransport{base: pool, http1: http1}
}

type s3ProtocolTransport struct {
	base, http1 *http.Transport
}

func (t *s3ProtocolTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if jdOSSHTTPS(request.URL) {
		return t.http1.RoundTrip(request)
	}
	return t.base.RoundTrip(request)
}

func (t *s3ProtocolTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
	t.http1.CloseIdleConnections()
}

func configureS3HTTP1(pool *http.Transport) {
	pool.ForceAttemptHTTP2 = false
	pool.Protocols = new(http.Protocols)
	pool.Protocols.SetHTTP1(true)
	pool.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if pool.TLSClientConfig == nil {
		pool.TLSClientConfig = &tls.Config{}
	} else {
		pool.TLSClientConfig = pool.TLSClientConfig.Clone()
	}
	// Clone can inherit h2 ALPN from an already-used transport. Disabling the
	// protocol alone would then negotiate h2 but send HTTP/1.1 on that socket.
	pool.TLSClientConfig.NextProtos = []string{"http/1.1"}
}

func jdOSSHTTPS(endpoint *url.URL) bool {
	if endpoint == nil || endpoint.Scheme != "https" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(endpoint.Hostname()), ".")
	const suffix = ".jdcloud-oss.com"
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	prefix := strings.TrimSuffix(host, suffix)
	for _, label := range strings.Split(prefix, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
