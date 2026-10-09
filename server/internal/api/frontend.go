package api

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// WithFrontend serves an explicitly selected static build alongside the API.
// It never exposes the project directory or local API-key files.
func WithFrontend(api http.Handler, directory string) (http.Handler, error) {
	files := os.DirFS(directory)
	index, err := fs.Stat(files, "index.html")
	if err != nil || !index.Mode().IsRegular() {
		return nil, fmt.Errorf("frontend build missing: build the frontend before setting -web-dir")
	}
	static := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") ||
			r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Use GET or HEAD for frontend assets.", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		info, err := fs.Stat(files, name)
		if err != nil && name != "." && !strings.HasSuffix(name, ".html") {
			cleanHTML := name + ".html"
			if htmlInfo, htmlErr := fs.Stat(files, cleanHTML); htmlErr == nil && htmlInfo.Mode().IsRegular() {
				name, info, err = cleanHTML, htmlInfo, nil
				r.URL.Path = "/" + cleanHTML
			}
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if info.IsDir() {
			index, err := fs.Stat(files, path.Join(name, "index.html"))
			if err != nil || !index.Mode().IsRegular() {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		static.ServeHTTP(w, r)
	}), nil
}
