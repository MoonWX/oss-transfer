package webui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Vite names files under assets/ by content hash, so a URL never changes
// meaning and browsers may keep it for a year without revalidating.
const immutableCache = "public, max-age=31536000, immutable"

// Small files gain little from compression and fonts are already compressed.
const minCompressSize = 1024

type asset struct {
	modTime     time.Time
	size        int64
	contentType string
	etag        string
	plain       []byte
	gzip        []byte
}

func Handler(files fs.FS) http.Handler {
	var mu sync.Mutex
	cache := map[string]*asset{}
	load := func(name string) (*asset, error) {
		info, err := fs.Stat(files, name)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			return nil, fs.ErrNotExist
		}
		mu.Lock()
		defer mu.Unlock()
		// Development builds read frontend/dist from disk, so a rebuilt file
		// replaces the cached copy. Embedded files never change.
		if cached := cache[name]; cached != nil && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
			return cached, nil
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		item := &asset{
			modTime: info.ModTime(),
			size:    info.Size(),
			etag:    `"` + hex.EncodeToString(sum[:16]) + `"`,
			plain:   data,
		}
		item.contentType = mime.TypeByExtension(path.Ext(name))
		if item.contentType == "" {
			item.contentType = http.DetectContentType(data)
		}
		if compressible(item.contentType) && len(data) >= minCompressSize {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(data)
			zw.Close()
			if buf.Len() < len(data) {
				item.gzip = buf.Bytes()
			}
		}
		cache[name] = item
		return item, nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		item, err := load(name)
		if err != nil {
			// Page routes retain the shared React layout. Missing resources do
			// not receive index.html disguised as JavaScript or a stylesheet.
			if path.Ext(name) != "" || strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)
				return
			}
			if item, err = load("index.html"); err != nil {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
		}
		header := w.Header()
		if strings.HasPrefix(name, "assets/") {
			header.Set("Cache-Control", immutableCache)
		} else {
			header.Set("Cache-Control", "no-cache")
		}
		header.Set("Content-Type", item.contentType)
		body := item.plain
		etag := item.etag
		if item.gzip != nil {
			header.Add("Vary", "Accept-Encoding")
			if acceptsGzip(r.Header.Get("Accept-Encoding")) {
				header.Set("Content-Encoding", "gzip")
				body = item.gzip
				// A compressed representation needs its own validator.
				etag = strings.TrimSuffix(etag, `"`) + `-gz"`
			}
		}
		header.Set("ETag", etag)
		// ServeContent omits the length for encoded bodies; this one is known.
		// It replaces the value for ranges and drops it for 304 responses.
		header.Set("Content-Length", strconv.Itoa(len(body)))
		// ServeContent answers If-None-Match with 304 and handles HEAD. The zero
		// modification time of embedded files is ignored.
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	})
}

func compressible(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")
	switch strings.TrimSpace(base) {
	case "text/html", "text/css", "text/plain", "text/javascript", "application/javascript",
		"application/json", "image/svg+xml", "image/x-icon", "image/vnd.microsoft.icon":
		return true
	}
	return false
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") && strings.TrimSpace(coding) != "*" {
			continue
		}
		params = strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return params != "q=0" && params != "q=0.0" && params != "q=0.00" && params != "q=0.000"
	}
	return false
}
