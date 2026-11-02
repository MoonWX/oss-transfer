package webui

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func testFiles() fstest.MapFS {
	script := []byte(strings.Repeat("console.log('oss transfer');\n", 200))
	return fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><div id=root></div>" + strings.Repeat(" ", 2048))},
		"assets/index-abc.js": {Data: script},
		"assets/font.woff2":   {Data: bytes.Repeat([]byte{1, 2, 3}, 1000)},
		"favicon.ico":         {Data: []byte{0, 0, 1, 0}},
	}
}

func get(t *testing.T, h http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHashedAssetsAreCompressedAndCachedForever(t *testing.T) {
	files := testFiles()
	h := Handler(files)
	rec := get(t, h, "/assets/index-abc.js", map[string]string{"Accept-Encoding": "gzip, deflate"})
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != immutableCache {
		t.Fatalf("Cache-Control = %q", got)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("Content-Length = %q, body %d bytes", got, rec.Body.Len())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if !bytes.Equal(plain, files["assets/index-abc.js"].Data) {
		t.Fatal("decompressed body differs from the file")
	}

	etag := rec.Header().Get("ETag")
	again := get(t, h, "/assets/index-abc.js", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag})
	if again.Code != http.StatusNotModified {
		t.Fatalf("revalidation status %d", again.Code)
	}
}

func TestClientsWithoutGzipReceivePlainBytes(t *testing.T) {
	files := testFiles()
	h := Handler(files)
	for _, accept := range []string{"", "identity", "gzip;q=0", "br"} {
		rec := get(t, h, "/assets/index-abc.js", map[string]string{"Accept-Encoding": accept})
		if rec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%q: unexpected encoding %q", accept, rec.Header().Get("Content-Encoding"))
		}
		if !bytes.Equal(rec.Body.Bytes(), files["assets/index-abc.js"].Data) {
			t.Fatalf("%q: body differs", accept)
		}
		if rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("%q: Vary = %q", accept, rec.Header().Get("Vary"))
		}
	}
}

func TestIndexAndPageRoutesRevalidate(t *testing.T) {
	h := Handler(testFiles())
	for _, target := range []string{"/", "/tasks/12", "/index.html"} {
		rec := get(t, h, target, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s: Cache-Control = %q", target, got)
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: Content-Type = %q", target, rec.Header().Get("Content-Type"))
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: missing ETag", target)
		}
		if again := get(t, h, target, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag}); again.Code != http.StatusNotModified {
			t.Fatalf("%s: revalidation status %d", target, again.Code)
		}
	}
}

func TestMissingResourcesAndOtherMethods(t *testing.T) {
	h := Handler(testFiles())
	for _, target := range []string{"/assets/missing.js", "/missing.css", "/assets/nested/x"} {
		if rec := get(t, h, target, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", rec.Code)
	}
}

func TestPrecompressedFormatsAreSentAsIs(t *testing.T) {
	h := Handler(testFiles())
	rec := get(t, h, "/assets/font.woff2", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("font was recompressed")
	}
	if rec.Header().Get("Cache-Control") != immutableCache {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestHeadRequestsHaveNoBody(t *testing.T) {
	h := Handler(testFiles())
	req := httptest.NewRequest("HEAD", "/assets/index-abc.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD status %d body %d", rec.Code, rec.Body.Len())
	}
}
