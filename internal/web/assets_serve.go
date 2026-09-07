package web

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"
)

// devVersion is the version string of an unreleased local build. Assets are
// never cached hard under it, so editing them during development takes effect
// on the next reload.
const devVersion = "dev"

// buildTime anchors If-Modified-Since handling for embedded assets. Embedded
// files have no meaningful modification time of their own, so process start is
// used; combined with the version query string this is enough for correct
// revalidation.
var buildTime = time.Now()

// renderAsset reads an embedded asset and substitutes the build version. This
// is what makes cache invalidation work without a bundler: index.html
// references every asset as /static/app.js?v=<version>, so a new release
// changes every URL.
func renderAsset(fsys fs.FS, name, version string) ([]byte, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(raw, []byte("__VERSION__"), []byte(version)), nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell must always be revalidated, otherwise a stale index would keep
	// pointing at the previous version's assets.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", buildTime, bytes.NewReader(s.index))
}

func (s *Server) serveTemplated(body []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "", buildTime, bytes.NewReader(body))
	}
}

// staticTypes maps the extensions the frontend actually uses. Serving from an
// explicit list means an unexpected file type can never be served with a
// content type that would let it execute in the page.
var staticTypes = map[string]string{
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".ico":  "image/x-icon",
	".webp": "image/webp",
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}

	contentType, ok := staticTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		http.NotFound(w, r)
		return
	}

	raw, err := fs.ReadFile(s.static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Assets are addressed with a ?v=<version> query string, so a cached copy
	// can only ever belong to the release that requested it. Anything without
	// the query is treated as unversioned and revalidated instead.
	//
	// Unreleased builds are never cached hard: the version string does not
	// change between them, so an edited asset would otherwise stay stale in the
	// browser for a year.
	if r.URL.Query().Get("v") == s.version && s.version != "" && s.version != devVersion {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, name, buildTime, bytes.NewReader(raw))
}

// listLanguages returns the language codes available as translation files.
func listLanguages(fsys fs.FS) []string {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return []string{"en"}
	}

	var langs []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		code := strings.ToLower(strings.TrimSuffix(name, ".json"))
		if code != "" {
			langs = append(langs, code)
		}
	}
	if len(langs) == 0 {
		return []string{"en"}
	}
	slices.Sort(langs)
	return langs
}

// contextWithTimeout derives a request-scoped context with a deadline.
func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}
