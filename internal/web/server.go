// Package web wires the HTTP layer: routing, sessions, templates and the
// static assets, all embedded in the binary.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/o5ten/cinevote/internal/config"
	"github.com/o5ten/cinevote/internal/demo"
	"github.com/o5ten/cinevote/internal/poster"
	"github.com/o5ten/cinevote/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	sessionCookie = "cinevote_session"
	flashCookie   = "cinevote_flash"

	// OMDbKeyURL is where a free API key comes from. Shown in the UI whenever
	// poster lookups are switched off, so nobody has to go hunting for it.
	OMDbKeyURL = "https://www.omdbapi.com/apikey.aspx"

	// TMDBKeyURL is where the key for "films like this one" comes from. OMDb
	// has no recommendation endpoint, so that half needs TMDB.
	TMDBKeyURL = "https://www.themoviedb.org/settings/api"
)

type Server struct {
	cfg     config.Config
	store   *store.Store
	posters *poster.Service
	log     *slog.Logger
	tmpl    map[string]*template.Template
	mux     *http.ServeMux
	logins  *throttle
	// assetVersion busts the browser cache when the CSS or JS changes. Without
	// it, a deploy leaves everyone on the old assets until max-age expires.
	assetVersion string
}

func New(cfg config.Config, st *store.Store, pc *poster.Service, log *slog.Logger) (*Server, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	version, err := hashAssets()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:          cfg,
		store:        st,
		posters:      pc,
		log:          log,
		tmpl:         tmpl,
		logins:       newThrottle(10, 15*time.Minute),
		assetVersion: version,
	}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.recoverer(s.requestLog(s.securityHeaders(s.mux))).ServeHTTP(w, r)
}

func (s *Server) routes() {
	mux := http.NewServeMux()

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Sprintf("static fs: %v", err)) // embedded, cannot fail at runtime
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static)))))

	mux.HandleFunc("GET /healthz", s.handleHealth)

	mux.HandleFunc("GET /{$}", s.requireUser(s.handleIndex))
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /register", s.handleRegisterForm)
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("POST /logout", s.requireUser(s.handleLogout))

	mux.HandleFunc("POST /movies", s.requireUser(s.handleAddMovie))
	mux.HandleFunc("POST /movies/{id}/vote", s.requireUser(s.handleVote))
	mux.HandleFunc("POST /movies/{id}/unvote", s.requireUser(s.handleUnvote))
	mux.HandleFunc("POST /movies/{id}/seen", s.requireAdmin(s.handleSetSeen))
	mux.HandleFunc("POST /movies/{id}/delete", s.requireUser(s.handleDeleteMovie))
	mux.HandleFunc("GET /movies/{id}/similar", s.requireUser(s.handleSimilar))

	mux.HandleFunc("GET /admin", s.requireAdmin(s.handleAdmin))
	mux.HandleFunc("POST /admin/users/{id}/delete", s.requireAdmin(s.handleDeleteUser))

	mux.HandleFunc("GET /api/search", s.requireUser(s.handleSearch))

	mux.HandleFunc("/", s.handleNotFound)

	s.mux = mux
}

/* ----------------------------------------------------------- middleware --- */

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxCSRF
)

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxUser).(*store.User)
	return u
}

func csrfFrom(ctx context.Context) string {
	t, _ := ctx.Value(ctxCSRF).(string)
	return t
}

// session resolves the cookie into a user, or returns nil when the visitor is
// anonymous or the session has expired.
func (s *Server) session(r *http.Request) (*store.User, string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, ""
	}
	user, csrf, err := s.store.Session(r.Context(), c.Value)
	if err != nil {
		return nil, ""
	}
	return user, csrf
}

func (s *Server) withUser(r *http.Request, u *store.User, csrf string) *http.Request {
	ctx := context.WithValue(r.Context(), ctxUser, u)
	ctx = context.WithValue(ctx, ctxCSRF, csrf)
	return r.WithContext(ctx)
}

// requireUser gates a handler behind a valid session. GETs bounce to the login
// page, everything else gets a plain 401 so a stale form post is obvious.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, csrf := s.session(r)
		if user == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "logga in igen", http.StatusUnauthorized)
			return
		}
		r = s.withUser(r, user, csrf)
		if r.Method != http.MethodGet && !s.checkCSRF(r, csrf) {
			http.Error(w, "ogiltig eller utgången formulärtoken, ladda om sidan", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r.Context()); u == nil || !u.IsAdmin {
			s.render(w, r, http.StatusForbidden, "error.html", map[string]any{
				"Heading": "Endast för admin",
				"Message": "Den här sidan är bara tillgänglig för administratören.",
			})
			return
		}
		next(w, r)
	})
}

func (s *Server) checkCSRF(r *http.Request, want string) bool {
	if want == "" {
		return false
	}
	got := r.FormValue("csrf")
	if got == "" {
		got = r.Header.Get("X-CSRF-Token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", rec)
				http.Error(w, "internt fel", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			return // too noisy to be useful
		}
		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).Round(time.Millisecond).String())
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		// Posters are hotlinked from TMDB (or wherever a user pasted them), so
		// images may come from any https host; scripts and styles may not.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' https: data:; script-src 'self'; "+
				"style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// cacheControl lets browsers keep assets for a long time. That is only safe
// because every asset URL carries a content hash (see hashAssets), so changed
// files are requested under a new URL.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Unversioned request: allow caching, but make it revalidate.
			w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		}
		next.ServeHTTP(w, r)
	})
}

// hashAssets fingerprints the embedded static files. The result goes on every
// asset URL as ?v=..., which is what makes a long cache lifetime safe.
func hashAssets() (string, error) {
	sum := sha256.New()
	err := fs.WalkDir(staticFS, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := staticFS.ReadFile(path)
		if err != nil {
			return err
		}
		sum.Write([]byte(path))
		sum.Write(body)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash static assets: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

/* -------------------------------------------------------------- throttle --- */

// throttle is a tiny fixed-window limiter, enough to make password guessing
// against a handful of friend accounts pointless.
type throttle struct {
	mu     sync.Mutex
	hits   map[string]*window
	limit  int
	period time.Duration
}

type window struct {
	count int
	reset time.Time
}

func newThrottle(limit int, period time.Duration) *throttle {
	return &throttle{hits: make(map[string]*window), limit: limit, period: period}
}

// allow records an attempt and reports whether it is within the limit.
func (t *throttle) allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	for k, w := range t.hits { // opportunistic cleanup, the map stays tiny
		if now.After(w.reset) {
			delete(t.hits, k)
		}
	}
	w, ok := t.hits[key]
	if !ok || now.After(w.reset) {
		t.hits[key] = &window{count: 1, reset: now.Add(t.period)}
		return true
	}
	w.count++
	return w.count <= t.limit
}

func (t *throttle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.hits, key)
}

/* ------------------------------------------------------------- templates --- */

var funcs = template.FuncMap{
	"initials": func(title string) string {
		var out []rune
		for _, word := range strings.Fields(title) {
			out = append(out, []rune(strings.ToUpper(word))[0])
			if len(out) == 2 {
				break
			}
		}
		return string(out)
	},
	"plural": func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	},
	"date": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format("2006-01-02")
	},
	"join": func(items []string, sep string) string { return strings.Join(items, sep) },
	// seq lets a template loop a fixed number of times, e.g. the vote dots.
	"seq": func(n int) []int {
		out := make([]int, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, i)
		}
		return out
	},
	"medal": func(rank int) string {
		switch rank {
		case 1:
			return "\U0001F947"
		case 2:
			return "\U0001F948"
		case 3:
			return "\U0001F949"
		}
		return ""
	},
	// dict builds a map so one template can be reused with extra context,
	// e.g. {{template "card" (dict "M" $m "Variant" "top" "Root" $)}}.
	"dict": func(pairs ...any) (map[string]any, error) {
		if len(pairs)%2 != 0 {
			return nil, errors.New("dict needs an even number of arguments")
		}
		out := make(map[string]any, len(pairs)/2)
		for i := 0; i < len(pairs); i += 2 {
			key, ok := pairs[i].(string)
			if !ok {
				return nil, fmt.Errorf("dict key %d is not a string", i)
			}
			out[key] = pairs[i+1]
		}
		return out, nil
	},
	// hueClass picks one of six poster-placeholder gradients. Inline styles are
	// blocked by our CSP, so the variation has to live in classes.
	"hueClass": func(id int64) string { return fmt.Sprintf("ph-%d", id%6) },
}

func parseTemplates() (map[string]*template.Template, error) {
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := make(map[string]*template.Template)
	for _, page := range pages {
		name := strings.TrimPrefix(page, "templates/")
		if name == "layout.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", page)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out[name] = t
	}
	if len(out) == 0 {
		return nil, errors.New("no templates found")
	}
	return out, nil
}

// render executes a page inside the shared layout. Common fields (user, CSRF
// token, flash message) are merged in here so handlers only pass their own data.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	t, ok := s.tmpl[page]
	if !ok {
		s.log.Error("unknown template", "page", page)
		http.Error(w, "internt fel", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["SiteName"] = s.cfg.SiteName
	data["User"] = userFrom(r.Context())
	data["CSRF"] = csrfFrom(r.Context())
	data["MaxVotes"] = s.cfg.MaxVotes
	data["LookupEnabled"] = s.posters.Enabled()
	data["LookupSource"] = s.posters.SourceLabel()
	data["AssetVersion"] = s.assetVersion
	data["OMDbKeyURL"] = OMDbKeyURL
	data["TMDBKeyURL"] = TMDBKeyURL
	data["TMDBEnabled"] = s.posters.RecommendationsEnabled()
	data["Demo"] = s.cfg.Demo
	if s.cfg.Demo {
		data["DemoPassword"] = demo.Password
		data["DemoAccounts"] = demo.Accounts(s.cfg.AdminUsername)
	}
	data["Flash"] = s.takeFlash(w, r)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		// Headers are already out; all we can do is log it.
		s.log.Error("render template", "page", page, "err", err)
	}
}

/* ----------------------------------------------------------------- flash --- */

type flashMsg struct {
	Level string // "ok" | "error"
	Text  string
}

func (s *Server) setFlash(w http.ResponseWriter, level, text string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(level + "|" + text)),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60,
	})
}

func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *flashMsg {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	level, text, ok := strings.Cut(string(raw), "|")
	if !ok || text == "" {
		return nil
	}
	if level != "ok" && level != "error" {
		level = "ok"
	}
	return &flashMsg{Level: level, Text: text}
}

/* --------------------------------------------------------------- helpers --- */

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id %q", r.PathValue("id"))
	}
	return id, nil
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
