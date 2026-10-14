package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

func Handler(files fs.FS) http.Handler {
	static := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(files, name); err != nil {
			// Page routes retain the shared React layout. Missing resources do
			// not receive index.html disguised as JavaScript or a stylesheet.
			if path.Ext(name) != "" || strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)
				return
			}
			clone := r.Clone(r.Context())
			w.Header().Set("Cache-Control", "no-cache")
			clone.URL.Path = "/"
			static.ServeHTTP(w, clone)
			return
		}
		if name == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		static.ServeHTTP(w, r)
	})
}
