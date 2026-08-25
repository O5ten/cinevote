package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikaelo/cinevote/internal/auth"
	"github.com/mikaelo/cinevote/internal/config"
	"github.com/mikaelo/cinevote/internal/demo"
	"github.com/mikaelo/cinevote/internal/poster"
	"github.com/mikaelo/cinevote/internal/store"
)

// app is a running CineVote with metadata lookups switched off, so tests never
// touch the network.
type app struct {
	t     *testing.T
	ts    *httptest.Server
	store *store.Store
	cfg   config.Config
}

func newApp(t *testing.T, tweak func(*config.Config)) *app {
	t.Helper()

	cfg := config.Config{
		DBPath:        filepath.Join(t.TempDir(), "test.db"),
		SiteName:      "CineVote",
		AdminUsername: "admin",
		MaxVotes:      5,
		SessionTTL:    time.Hour,
	}
	if tweak != nil {
		tweak(&cfg)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	st.MaxVotes = cfg.MaxVotes
	t.Cleanup(func() { st.Close() })

	posters, err := poster.New("none", "", "")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, st, posters, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &app{t: t, ts: ts, store: st, cfg: cfg}
}

// client is one logged-in browser: it keeps cookies and tracks the CSRF token
// found on the last page it loaded.
type client struct {
	t    *testing.T
	app  *app
	http *http.Client
	csrf string
}

func (a *app) client() *client {
	a.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		a.t.Fatal(err)
	}
	return &client{t: a.t, app: a, http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (c *client) get(path string) (int, string) {
	c.t.Helper()
	resp, err := c.http.Get(c.app.ts.URL + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	if m := csrfRe.FindStringSubmatch(string(body)); m != nil {
		c.csrf = m[1]
	}
	return resp.StatusCode, string(body)
}

// post submits a form, including the CSRF token from the last page load unless
// the caller already set one.
func (c *client) post(path string, form url.Values) (int, string) {
	c.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if _, ok := form["csrf"]; !ok && c.csrf != "" {
		form.Set("csrf", c.csrf)
	}
	resp, err := c.http.PostForm(c.app.ts.URL+path, form)
	if err != nil {
		c.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	if m := csrfRe.FindStringSubmatch(string(body)); m != nil {
		c.csrf = m[1]
	}
	return resp.StatusCode, string(body)
}

func (c *client) register(name, password string) {
	c.t.Helper()
	c.get("/register")
	status, body := c.post("/register", url.Values{
		"username": {name},
		"password": {password},
	})
	if status != http.StatusOK {
		c.t.Fatalf("register %s: status %d\n%s", name, status, first(body, 400))
	}
}

// loginAdmin creates the single admin account straight in the store (the way
// the binary bootstraps it) and logs in.
func (c *client) loginAdmin(password string) {
	c.t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.app.store.UpsertAdmin(context.Background(), c.app.cfg.AdminUsername, hash); err != nil {
		c.t.Fatal(err)
	}
	c.get("/login")
	status, body := c.post("/login", url.Values{
		"username": {c.app.cfg.AdminUsername},
		"password": {password},
	})
	if status != http.StatusOK {
		c.t.Fatalf("admin login: status %d\n%s", status, first(body, 400))
	}
}

func (c *client) addMovie(title string) {
	c.t.Helper()
	c.get("/")
	status, body := c.post("/movies", url.Values{"title": {title}})
	if status != http.StatusOK {
		c.t.Fatalf("add %s: status %d\n%s", title, status, first(body, 400))
	}
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func mustContain(t *testing.T, body, want, what string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("%s: page does not contain %q", what, want)
	}
}

/* ------------------------------------------------------------------ tests --- */

func TestHealthz(t *testing.T) {
	a := newApp(t, nil)
	status, body := a.client().get("/healthz")
	if status != http.StatusOK || !strings.Contains(body, `"ok"`) {
		t.Fatalf("healthz = %d %q", status, body)
	}
}

func TestBoardRequiresLogin(t *testing.T) {
	a := newApp(t, nil)
	c := a.client()

	// The client follows the redirect, so we should land on the login form.
	status, body := c.get("/")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mustContain(t, body, "Logga in", "anonymous visitor")

	// A form post without a session must not be silently redirected.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.PostForm(a.ts.URL+"/movies", url.Values{"title": {"Sneaky"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous POST /movies = %d, want 401", resp.StatusCode)
	}
}

func TestCSRFTokenRequired(t *testing.T) {
	a := newApp(t, nil)
	c := a.client()
	c.register("Ada", "hunter2hunter2")

	status, _ := c.post("/movies", url.Values{"title": {"Alien"}, "csrf": {"not-the-token"}})
	if status != http.StatusForbidden {
		t.Fatalf("bad CSRF token accepted: status %d", status)
	}
}

func TestRegistrationCodeEnforced(t *testing.T) {
	a := newApp(t, func(c *config.Config) { c.RegistrationCode = "filmkväll" })
	c := a.client()

	c.get("/register")
	status, body := c.post("/register", url.Values{
		"username": {"Ada"},
		"password": {"hunter2hunter2"},
		"code":     {"wrong"},
	})
	if status != http.StatusForbidden {
		t.Fatalf("wrong code accepted: status %d", status)
	}
	mustContain(t, body, "Fel inbjudningskod", "wrong invite code")

	status, body = c.post("/register", url.Values{
		"username": {"Ada"},
		"password": {"hunter2hunter2"},
		"code":     {"filmkväll"},
	})
	if status != http.StatusOK {
		t.Fatalf("correct code rejected: status %d\n%s", status, first(body, 300))
	}
	mustContain(t, body, "Ada", "signed-in header")
}

func TestAdminUsernameReserved(t *testing.T) {
	a := newApp(t, nil)
	c := a.client()
	c.get("/register")
	status, body := c.post("/register", url.Values{
		"username": {"Admin"},
		"password": {"hunter2hunter2"},
	})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	mustContain(t, body, "reserverat", "reserved username")
}

func TestVotingFlow(t *testing.T) {
	a := newApp(t, nil)
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	_, body := ada.get("/")
	mustContain(t, body, "5 / 5 röster kvar", "fresh account")

	ada.addMovie("Alien")
	_, body = ada.get("/")
	mustContain(t, body, "Alien", "board after suggesting")
	mustContain(t, body, "Föreslagen av Ada", "attribution")

	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil || len(movies) != 1 {
		t.Fatalf("expected one movie, got %v (%v)", movies, err)
	}
	id := movies[0].ID

	_, body = ada.post("/movies/"+itoa64(id)+"/vote", nil)
	mustContain(t, body, "4 / 5 röster kvar", "after voting")
	mustContain(t, body, "Din röst", "own vote marker")
	mustContain(t, body, "Ta tillbaka rösten", "unvote button")

	// A second vote on the same film is refused.
	_, body = ada.post("/movies/"+itoa64(id)+"/vote", nil)
	mustContain(t, body, "redan röstat", "double vote")

	_, body = ada.post("/movies/"+itoa64(id)+"/unvote", nil)
	mustContain(t, body, "5 / 5 röster kvar", "after taking the vote back")
}

func TestVoteBudgetIsEnforcedOverHTTP(t *testing.T) {
	a := newApp(t, func(c *config.Config) { c.MaxVotes = 2 })
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	for _, title := range []string{"A", "B", "C"} {
		ada.addMovie(title)
	}
	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		ada.post("/movies/"+itoa64(movies[i].ID)+"/vote", nil)
	}
	_, body := ada.post("/movies/"+itoa64(movies[2].ID)+"/vote", nil)
	mustContain(t, body, "röster är slut", "third vote with a budget of two")
	mustContain(t, body, "Alla röster utlagda", "spent-budget pill")
}

func TestDuplicateSuggestionRejected(t *testing.T) {
	a := newApp(t, nil)
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	ada.addMovie("Arrival")
	_, body := ada.post("/movies", url.Values{"title": {"arrival"}})
	mustContain(t, body, "ligger redan på listan", "duplicate suggestion")
}

func TestOnlyAdminReachesAdminPage(t *testing.T) {
	a := newApp(t, nil)
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	status, body := ada.get("/admin")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	mustContain(t, body, "Endast för admin", "non-admin on /admin")
}

// The requirement that ties it together: the admin marks a movie as seen and
// everyone who voted for it gets that vote back.
func TestAdminMarksSeenAndVotesComeBack(t *testing.T) {
	a := newApp(t, nil)

	ada := a.client()
	ada.register("Ada", "hunter2hunter2")
	ada.addMovie("Stalker")

	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil || len(movies) != 1 {
		t.Fatalf("expected one movie: %v %v", movies, err)
	}
	id := movies[0].ID

	_, body := ada.post("/movies/"+itoa64(id)+"/vote", nil)
	mustContain(t, body, "4 / 5 röster kvar", "after voting")

	admin := a.client()
	admin.loginAdmin("supersecret123")
	_, body = admin.get("/admin")
	mustContain(t, body, "Stalker", "admin movie table")

	_, body = admin.post("/movies/"+itoa64(id)+"/seen", url.Values{"seen": {"1"}})
	mustContain(t, body, "markerad som sedd", "seen confirmation")

	// Ada has her vote back, and the film has moved to the seen section.
	_, body = ada.get("/")
	mustContain(t, body, "5 / 5 röster kvar", "vote returned")
	mustContain(t, body, "Sedda filmer", "seen section")
	mustContain(t, body, "rösterna är återlämnade", "seen section note")

	// Voting for a watched film is refused.
	_, body = ada.post("/movies/"+itoa64(id)+"/vote", nil)
	mustContain(t, body, "redan sedd", "voting on a seen movie")
}

func TestTopThreeAreHighlighted(t *testing.T) {
	a := newApp(t, nil)

	// Four films, each with a different number of backers.
	voters := []*client{}
	for _, name := range []string{"Ada", "Bo", "Cleo"} {
		c := a.client()
		c.register(name, "hunter2hunter2")
		voters = append(voters, c)
	}
	for _, title := range []string{"First", "Second", "Third", "Fourth"} {
		voters[0].addMovie(title)
	}

	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]int64{}
	for _, m := range movies {
		byTitle[m.Title] = m.ID
	}

	// First: 3 votes, Second: 2, Third: 1, Fourth: none.
	for _, c := range voters {
		c.post("/movies/"+itoa64(byTitle["First"])+"/vote", nil)
	}
	for _, c := range voters[:2] {
		c.post("/movies/"+itoa64(byTitle["Second"])+"/vote", nil)
	}
	voters[0].post("/movies/"+itoa64(byTitle["Third"])+"/vote", nil)

	_, body := voters[0].get("/")
	mustContain(t, body, "de 3 mest röstade", "podium heading")

	podium := section(body, `<div class="podium">`, "</section>")
	for _, want := range []string{"First", "Second", "Third"} {
		if !strings.Contains(podium, want) {
			t.Errorf("%s should be on the podium", want)
		}
	}
	if strings.Contains(podium, "Fourth") {
		t.Error("a film with no votes should not be on the podium")
	}
	// The leader is announced at the top of the page.
	mustContain(t, body, "Nästa filmkväll", "hero")
	if i, j := strings.Index(body, "Nästa filmkväll"), strings.Index(body, "First"); i > j || j < 0 {
		t.Error("the leading film should be named in the hero section")
	}
}

func TestOwnSuggestionCanBeWithdrawnUntilVoted(t *testing.T) {
	a := newApp(t, nil)
	ada := a.client()
	bo := a.client()
	ada.register("Ada", "hunter2hunter2")
	bo.register("Bo", "hunter2hunter2")

	ada.addMovie("Tenet")
	movies, _ := a.store.Movies(context.Background(), 0)
	id := movies[0].ID

	// Someone else cannot remove it.
	_, body := bo.post("/movies/"+itoa64(id)+"/delete", nil)
	mustContain(t, body, "bara ta bort dina egna", "other user deleting")

	// With a vote on it, not even the suggester can.
	bo.post("/movies/"+itoa64(id)+"/vote", nil)
	_, body = ada.post("/movies/"+itoa64(id)+"/delete", nil)
	mustContain(t, body, "be admin ta bort", "deleting a film with votes")

	// Without votes, the suggester can withdraw it.
	ada.addMovie("Nope")
	movies, _ = a.store.Movies(context.Background(), 0)
	var nope int64
	for _, m := range movies {
		if m.Title == "Nope" {
			nope = m.ID
		}
	}
	_, body = ada.post("/movies/"+itoa64(nope)+"/delete", nil)
	mustContain(t, body, "är borttagen", "withdrawing an unvoted suggestion")
}

func TestSearchDisabledWithoutAPIKey(t *testing.T) {
	a := newApp(t, nil)
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	status, body := ada.get("/api/search?q=alien")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mustContain(t, body, `"enabled":false`, "search with no provider")
	mustContain(t, body, `"results":[]`, "empty result list")
}

// Demo mode has to be usable with no configuration at all: the login page
// lists the accounts and the shared password, and they actually work.
func TestDemoModeIsSelfExplanatory(t *testing.T) {
	a := newApp(t, func(c *config.Config) { c.Demo = true })

	// Same bootstrap order as the binary: admin first, then the seed.
	hash, err := auth.HashPassword(demo.Password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.UpsertAdmin(context.Background(), a.cfg.AdminUsername, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := demo.Seed(context.Background(), a.store, a.cfg.AdminUsername); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := a.client()
	_, body := c.get("/login")
	mustContain(t, body, "Demo", "demo banner")
	mustContain(t, body, demo.Password, "shared demo password")
	mustContain(t, body, "Demokonton", "demo account list")
	for _, acc := range demo.Accounts(a.cfg.AdminUsername) {
		mustContain(t, body, acc.Username, "demo account "+acc.Username)
	}

	// The advertised credentials must actually log in.
	status, body := c.post("/login", url.Values{
		"username": {"Anna"},
		"password": {demo.Password},
	})
	if status != http.StatusOK {
		t.Fatalf("demo login: status %d", status)
	}
	mustContain(t, body, "Nästa filmkväll", "seeded board")
	mustContain(t, body, "Parasite", "seeded film")
	mustContain(t, body, "Sedda filmer", "seeded watched films")
	mustContain(t, body, "de 3 mest röstade", "seeded podium")

	// And the admin login works too, since the page advertises it.
	admin := a.client()
	admin.get("/login")
	status, body = admin.post("/login", url.Values{
		"username": {a.cfg.AdminUsername},
		"password": {demo.Password},
	})
	if status != http.StatusOK {
		t.Fatalf("demo admin login: status %d", status)
	}
	status, body = admin.get("/admin")
	if status != http.StatusOK {
		t.Fatalf("demo admin page: status %d", status)
	}
	mustContain(t, body, "Filmer", "admin table")
}

func TestNoDemoBannerOutsideDemoMode(t *testing.T) {
	a := newApp(t, nil)
	_, body := a.client().get("/login")
	if strings.Contains(body, "Demokonton") || strings.Contains(body, demo.Password) {
		t.Error("a normal deployment must not advertise demo credentials")
	}
}

// With no API key configured the UI has to say where to get one.
func TestOMDbKeyLinkShownWhenLookupIsOff(t *testing.T) {
	a := newApp(t, nil)
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	_, body := ada.get("/")
	mustContain(t, body, OMDbKeyURL, "suggestion form")
	mustContain(t, body, "omdbapi.com/apikey.aspx", "readable link text")
}

func TestUnknownPathIs404(t *testing.T) {
	a := newApp(t, nil)
	status, body := a.client().get("/no-such-page")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	mustContain(t, body, "Sidan finns inte", "404 page")
}

func TestLogout(t *testing.T) {
	a := newApp(t, nil)
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	_, body := ada.post("/logout", nil)
	mustContain(t, body, "Logga in", "after logging out")

	_, body = ada.get("/")
	mustContain(t, body, "Logga in", "session should be gone")
}

func TestStaticAssetsAreServed(t *testing.T) {
	a := newApp(t, nil)
	for _, path := range []string{"/static/style.css", "/static/app.js", "/static/favicon.svg"} {
		status, body := a.client().get(path)
		if status != http.StatusOK || body == "" {
			t.Errorf("%s = %d (%d bytes)", path, status, len(body))
		}
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// section returns the slice of body between two markers, for asserting that a
// title appears inside a specific block rather than anywhere on the page.
func section(body, start, end string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return ""
	}
	rest := body[i+len(start):]
	if j := strings.Index(rest, end); j >= 0 {
		return rest[:j]
	}
	return rest
}
