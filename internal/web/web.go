// Package web serves the frontend and its JSON API.
//
// The UI is deliberately dependency-free: no bundler, no framework, no npm.
// Static assets are embedded in the binary and served with a version query
// string, so a new image release invalidates every cached asset without any
// build tooling.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/archive"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/capture"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/schedule"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/syncer"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/video"
)

//go:embed assets
var assets embed.FS

// authCookie is the name of the session cookie set after a successful token
// bootstrap.
const authCookie = "tl_session"

// Server is the HTTP frontend.
type Server struct {
	cfg       *config.Config
	store     *state.Store
	capturer  *capture.Capturer
	syncer    *syncer.Syncer
	browser   *archive.Browser
	scheduler *schedule.Scheduler
	video     *video.Renderer
	log       *slog.Logger
	version   string

	// proxy forwards archive requests to another instance when this one has no
	// archive mount of its own.
	proxy *http.Client

	// newestFrame locates the most recent archived image. It is injected so the
	// web package does not depend on the monitor package.
	newestFrame func() (string, time.Time, error)

	static  fs.FS
	i18n    fs.FS
	langs   []string
	index   []byte
	swJS    []byte
	webmani []byte

	// liveMu rate-limits live preview fetches so a reloading browser tab
	// cannot hammer the camera.
	liveMu   sync.Mutex
	liveLast time.Time
	liveData []byte
	liveTime time.Time
}

// Options bundles the collaborators the server needs.
type Options struct {
	Config    *config.Config
	State     *state.Store
	Capturer  *capture.Capturer
	Syncer    *syncer.Syncer
	Scheduler *schedule.Scheduler
	Video     *video.Renderer
	Log       *slog.Logger
	Version   string
	// NewestFrame locates the most recent archived image, used as the live
	// view's source when capture is disabled.
	NewestFrame func() (string, time.Time, error)
}

// New builds the server and prepares the embedded assets.
func New(opts Options) (*Server, error) {
	static, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:         opts.Config,
		store:       opts.State,
		capturer:    opts.Capturer,
		syncer:      opts.Syncer,
		browser:     archive.New(opts.Config),
		scheduler:   opts.Scheduler,
		video:       opts.Video,
		log:         opts.Log,
		version:     opts.Version,
		static:      static,
		proxy:       &http.Client{Timeout: 60 * time.Second},
		newestFrame: opts.NewestFrame,
	}

	// Translations may be overridden from disk so a new language can be added
	// without rebuilding the image.
	s.i18n = mustSub(static, "i18n")
	if dir := opts.Config.Web.I18nDir; dir != "" {
		if _, err := os.Stat(dir); err == nil {
			s.i18n = os.DirFS(dir)
			opts.Log.Info("serving translations from disk", "dir", dir)
		} else {
			opts.Log.Warn("WEB_I18N_DIR is not readable, falling back to built-in translations",
				"dir", dir, "error", err)
		}
	}
	s.langs = listLanguages(s.i18n)

	// Templated assets carry the build version so caches are invalidated on
	// every release.
	if s.index, err = renderAsset(static, "index.html", s.version); err != nil {
		return nil, err
	}
	if s.swJS, err = renderAsset(static, "sw.js", s.version); err != nil {
		return nil, err
	}
	if s.webmani, err = renderAsset(static, "manifest.webmanifest", s.version); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: liveness and metrics are for the container runtime and
	// the monitoring system, neither of which carries a session.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	if s.cfg.MetricsEnabled {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
	}

	// The service worker must be served from the root to control the whole
	// scope, and the manifest is fetched before any session exists.
	mux.HandleFunc("GET /sw.js", s.serveTemplated(s.swJS, "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /manifest.webmanifest", s.serveTemplated(s.webmani, "application/manifest+json"))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(s.handleStatic)))

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/languages", s.handleLanguages)
	mux.HandleFunc("GET /api/i18n/{lang}", s.handleTranslation)
	mux.HandleFunc("GET /api/latest.jpg", s.handleLatest)
	mux.HandleFunc("GET /api/live.jpg", s.handleLive)

	if s.cfg.Web.ArchiveEnabled {
		mux.HandleFunc("GET /api/archive/years", s.handleYears)
		mux.HandleFunc("GET /api/archive/years/{year}/months", s.handleMonths)
		mux.HandleFunc("GET /api/archive/months/{month}/days", s.handleDays)
		mux.HandleFunc("GET /api/archive/days/{day}/frames", s.handleFrames)
		mux.HandleFunc("GET /api/archive/days/{day}/frames/{name}", s.handleFrame)
		mux.HandleFunc("GET /api/archive/days/{day}/report", s.handleReport)
		if s.cfg.Web.ZipEnabled {
			mux.HandleFunc("GET /api/archive/days/{day}/download.zip", s.handleZip)
		}
		if s.video != nil && s.video.Available() {
			mux.HandleFunc("GET /api/archive/days/{day}/video.mp4", s.handleVideo)
		}
	}

	return s.securityHeaders(s.authenticate(mux))
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.cfg.Web.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Generous, because a slow client fetching a full day of frames should
		// not be cut off mid-download.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("web interface listening",
			"addr", s.cfg.Web.Addr, "auth", s.cfg.Web.AuthToken != "")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

// securityHeaders applies a strict policy to every response. The content
// security policy is only this tight because the frontend uses no inline
// scripts or styles and loads nothing from a third-party origin.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'none'; " +
		"script-src 'self'; " +
		// worker-src has its own fallback chain and would otherwise land on
		// default-src 'none', which blocks the service worker.
		"worker-src 'self'; " +
		"style-src 'self'; " +
		"img-src 'self' data: blob:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"manifest-src 'self'; " +
		"base-uri 'none'; " +
		"form-action 'none'; " +
		"frame-ancestors 'none'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		next.ServeHTTP(w, r)
	})
}

// authenticate gates the UI behind a shared token when one is configured.
// Three ways to present it are accepted: an Authorization: Bearer header for
// scripted access, a session cookie for normal browsing, and a one-time
// ?token= query parameter that exchanges itself for the cookie so the URL can
// be bookmarked once and forgotten.
func (s *Server) authenticate(next http.Handler) http.Handler {
	// Under Home Assistant ingress the Supervisor has already authenticated the
	// user before the request reaches this process, and the port is not
	// published, so a second credential would only be friction.
	if s.cfg.Web.AuthMode == config.AuthIngress {
		return next
	}

	token := s.cfg.Web.AuthToken
	if s.cfg.Web.AuthMode == config.AuthNone || token == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health and metrics stay reachable for the container runtime.
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		if provided := r.URL.Query().Get("token"); provided != "" {
			if !tokenMatches(provided, token) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     authCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				Secure:   r.TLS != nil,
				MaxAge:   int((90 * 24 * time.Hour).Seconds()),
			})
			// Redirect to the same path without the token, so it does not stay
			// in the address bar, in history or in a referrer.
			clean := *r.URL
			query := clean.Query()
			query.Del("token")
			clean.RawQuery = query.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
			return
		}

		if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
			if tokenMatches(strings.TrimPrefix(header, "Bearer "), token) {
				next.ServeHTTP(w, r)
				return
			}
		}

		if cookie, err := r.Cookie(authCookie); err == nil && tokenMatches(cookie.Value, token) {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// tokenMatches compares in constant time so the comparison leaks no timing
// information about the configured token.
func tokenMatches(provided, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return fsys
	}
	return sub
}
