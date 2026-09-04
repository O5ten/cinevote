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
	"strings"
	"testing"
	"time"

	"github.com/o5ten/cinevote/internal/auth"
	"github.com/o5ten/cinevote/internal/config"
	"github.com/o5ten/cinevote/internal/demo"
	"github.com/o5ten/cinevote/internal/mattermost"
	"github.com/o5ten/cinevote/internal/poster"
	"github.com/o5ten/cinevote/internal/store"
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
	return newAppWithChat(t, tweak, nil)
}

// newAppWithChat is newApp plus a Mattermost client, for the identity mode.
func newAppWithChat(t *testing.T, tweak func(*config.Config), chat *mattermost.Client) *app {
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
	srv, err := New(cfg, st, posters, chat, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// addMovieWith puts a film on the board with metadata the filters can bite on,
// bypassing the form so the test does not need a metadata provider.
func (a *app) seedMovie(m store.NewMovie) int64 {
	a.t.Helper()
	if m.SuggestedBy == 0 {
		user, err := a.store.UserByUsername(context.Background(), "Ada")
		if err != nil {
			a.t.Fatal(err)
		}
		m.SuggestedBy = user.ID
	}
	id, err := a.store.AddMovie(context.Background(), m)
	if err != nil {
		a.t.Fatalf("seed movie %s: %v", m.Title, err)
	}
	return id
}

func filterApp(t *testing.T) (*app, *client) {
	t.Helper()
	a := newApp(t, nil)
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	a.seedMovie(store.NewMovie{
		Title: "Dune: Part One", Year: "2021", Rating: "8.0", IMDbID: "tt1160419",
		Genres: "Action, Adventure, Drama", Director: "Denis Villeneuve",
		Actors: "Timothée Chalamet, Rebecca Ferguson, Zendaya",
	})
	a.seedMovie(store.NewMovie{
		Title: "Blade Runner 2049", Year: "2017", Rating: "8.1", IMDbID: "tt1856101",
		Genres: "Action, Drama, Mystery", Director: "Denis Villeneuve",
		Actors: "Ryan Gosling, Harrison Ford, Ana de Armas",
	})
	a.seedMovie(store.NewMovie{
		Title: "The Grand Budapest Hotel", Year: "2014", Rating: "8.1",
		Genres: "Comedy, Drama", Director: "Wes Anderson",
		Actors: "Ralph Fiennes, F. Murray Abraham",
	})
	a.seedMovie(store.NewMovie{
		Title: "Sharknado", Year: "2013", Rating: "3.3",
		Genres: "Action, Comedy, Horror", Director: "Anthony C. Ferrante",
		Actors: "Ian Ziering, Tara Reid",
	})
	return a, ada
}

func TestBoardFilters(t *testing.T) {
	a, ada := filterApp(t)
	_ = a

	cases := []struct {
		name     string
		path     string
		want     []string
		unwanted []string
	}{
		{
			name: "genre", path: "/?genre=Mystery",
			want: []string{"Blade Runner 2049"}, unwanted: []string{"Sharknado", "The Grand Budapest Hotel"},
		},
		{
			name: "director", path: "/?director=Denis+Villeneuve",
			want: []string{"Dune: Part One", "Blade Runner 2049"}, unwanted: []string{"Sharknado"},
		},
		{
			name: "minimum rating", path: "/?min_rating=8",
			want: []string{"Dune: Part One", "Blade Runner 2049"}, unwanted: []string{"Sharknado"},
		},
		{
			name: "free text over cast", path: "/?q=gosling",
			want: []string{"Blade Runner 2049"}, unwanted: []string{"Dune: Part One"},
		},
		{
			name: "free text over title", path: "/?q=budapest",
			want: []string{"The Grand Budapest Hotel"}, unwanted: []string{"Sharknado"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := ada.get(tc.path)
			if status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			mustContain(t, body, "Träffar", "filtered heading")
			for _, want := range tc.want {
				mustContain(t, body, want, tc.name+" should match")
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(body, unwanted) {
					t.Errorf("%s: %q should have been filtered out", tc.name, unwanted)
				}
			}
			// A filtered board drops the podium: those ranks describe the whole
			// list, not the filtered slice.
			if strings.Contains(body, "de 3 mest röstade") {
				t.Error("the podium should be hidden while filtering")
			}
		})
	}
}

func TestBoardSorting(t *testing.T) {
	_, ada := filterApp(t)

	_, body := ada.get("/?sort=title")
	dune := strings.Index(body, "Dune: Part One")
	blade := strings.Index(body, "Blade Runner 2049")
	if blade < 0 || dune < 0 || blade > dune {
		t.Error("sort=title should put Blade Runner before Dune")
	}

	_, body = ada.get("/?sort=rating")
	if i, j := strings.Index(body, "Blade Runner 2049"), strings.Index(body, "Sharknado"); i < 0 || j < 0 || i > j {
		t.Error("sort=rating should put the 8.1 film before the 3.3 one")
	}
}

func TestUnfilteredBoardKeepsThePodium(t *testing.T) {
	_, ada := filterApp(t)
	_, body := ada.get("/")
	if strings.Contains(body, "Träffar") {
		t.Error("an unfiltered board should not render the filtered view")
	}
	mustContain(t, body, "Sök i listan", "filter bar is always available")
}

func TestFilterWithNoMatchesExplainsItself(t *testing.T) {
	_, ada := filterApp(t)
	_, body := ada.get("/?q=ingenting-alls")
	mustContain(t, body, "Inga filmer matchar filtret", "empty filter result")
}

func TestSimilarPage(t *testing.T) {
	a, ada := filterApp(t)

	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var dune int64
	for _, m := range movies {
		if m.Title == "Dune: Part One" {
			dune = m.ID
		}
	}

	status, body := ada.get("/movies/" + itoa64(dune) + "/similar")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mustContain(t, body, "Liknande filmer", "page heading")
	mustContain(t, body, "Blade Runner 2049", "same-director match")
	mustContain(t, body, "Samma regissör", "the reason is spelled out")
	if strings.Contains(body, "Sharknado") {
		t.Error("a film with nothing in common should not be suggested")
	}

	// Without a TMDB key the page says how to enable outside tips instead of
	// pretending there are none.
	mustContain(t, body, TMDBKeyURL, "how to enable recommendations")
}

func TestSimilarPageForUnknownMovie(t *testing.T) {
	_, ada := filterApp(t)
	status, body := ada.get("/movies/9999/similar")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	mustContain(t, body, "Filmen finns inte", "missing movie")
}

// The card layout is what everyone starts on; the table is opt-in.
func TestCardViewIsTheDefault(t *testing.T) {
	_, ada := filterApp(t)

	_, body := ada.get("/")
	mustContain(t, body, `class="grid"`, "card layout")
	if strings.Contains(body, "table-board") {
		t.Error("the table should not render until it is asked for")
	}
	// The toggle offers the other view.
	mustContain(t, body, `href="/?view=list"`, "link to the list view")
}

func TestListViewRendersAVotableTable(t *testing.T) {
	a, ada := filterApp(t)

	// One vote, so there is a leader to medal.
	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ada.get("/")
	ada.post("/movies/"+itoa64(movies[0].ID)+"/vote", nil)

	_, body := ada.get("/?view=list")
	mustContain(t, body, "table-board", "table layout")
	mustContain(t, body, "Alla förslag", "table heading")
	if strings.Contains(body, `class="podium"`) {
		t.Error("the podium belongs to the card layout only")
	}
	// Every film is a row, and the top three keep their medals.
	for _, title := range []string{"Dune: Part One", "Blade Runner 2049", "Sharknado"} {
		mustContain(t, body, title, "row for "+title)
	}
	mustContain(t, body, "🥇", "gold medal on the leading row")

	// Rows carry vote forms that return to the list view.
	id := itoa64(movies[1].ID)
	mustContain(t, body, `action="/movies/`+id+`/vote"`, "vote form in the table")
	mustContain(t, body, `name="back" value="view=list"`, "the row remembers the view")
}

// Voting from the table has to come back to the table, at the same row.
func TestVotingFromTheListViewStaysInIt(t *testing.T) {
	a, ada := filterApp(t)

	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	id := itoa64(movies[0].ID)

	ada.get("/?view=list")
	noRedirect := *ada.http
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.PostForm(a.ts.URL+"/movies/"+id+"/vote", url.Values{
		"csrf": {ada.csrf},
		"back": {"view=list"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Location"); got != "/?view=list#film-"+id {
		t.Fatalf("Location = %q, want the list view anchored at the film", got)
	}

	// And the vote landed.
	_, body := ada.get("/?view=list")
	mustContain(t, body, "4 / 5 röster kvar", "vote counted")
	mustContain(t, body, "Ta tillbaka", "the row now offers to undo")
}

// Filters have to survive the round trip too, not just the view.
func TestVotingKeepsActiveFilters(t *testing.T) {
	a, ada := filterApp(t)
	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	id := itoa64(movies[0].ID)

	noRedirect := *ada.http
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ada.get("/?view=list&genre=Drama&sort=rating")
	resp, err := noRedirect.PostForm(a.ts.URL+"/movies/"+id+"/vote", url.Values{
		"csrf": {ada.csrf},
		"back": {"view=list&genre=Drama&sort=rating"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := resp.Header.Get("Location")
	for _, want := range []string{"view=list", "genre=Drama", "sort=rating", "#film-" + id} {
		if !strings.Contains(got, want) {
			t.Errorf("Location %q lost %q", got, want)
		}
	}
}

// A crafted return target must not turn a vote into an open redirect.
func TestReturnTargetIsSanitised(t *testing.T) {
	a, ada := filterApp(t)
	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	id := itoa64(movies[0].ID)

	noRedirect := *ada.http
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ada.get("/")
	resp, err := noRedirect.PostForm(a.ts.URL+"/movies/"+id+"/vote", url.Values{
		"csrf":   {ada.csrf},
		"return": {"https://evil.example/"},
		"back":   {"view=list&evil=payload&q=dune"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := resp.Header.Get("Location")
	if strings.Contains(got, "evil") || !strings.HasPrefix(got, "/?") {
		t.Fatalf("Location = %q, want a sanitised local path", got)
	}
	if !strings.Contains(got, "view=list") || !strings.Contains(got, "q=dune") {
		t.Errorf("Location = %q should still keep the known parameters", got)
	}
}

// Choosing a view sticks, so clicking around does not throw you back to cards.
func TestViewChoiceIsRemembered(t *testing.T) {
	_, ada := filterApp(t)

	if _, body := ada.get("/?view=list"); !strings.Contains(body, "table-board") {
		t.Fatal("list view did not render")
	}
	// No view parameter this time: the earlier choice should still apply.
	_, body := ada.get("/")
	mustContain(t, body, "table-board", "remembered list view")

	// And it can be switched back.
	if _, body := ada.get("/?view=cards"); strings.Contains(body, "table-board") {
		t.Error("switching back to cards did not take effect")
	}
	if _, body := ada.get("/"); strings.Contains(body, "table-board") {
		t.Error("the switch back to cards was not remembered")
	}
}

// With no key configured there is no quota to report.
func TestQuotaHiddenWithoutAProvider(t *testing.T) {
	_, ada := filterApp(t)
	_, body := ada.get("/")
	if strings.Contains(body, "anrop kvar idag") {
		t.Error("a deployment with no API key should not show an API quota")
	}
}

// What gets premiered when two films are level: the better-reviewed one, and
// between equal reviews the newer film.
func TestPremiereTieBreakOnTheBoard(t *testing.T) {
	a := newApp(t, nil)
	ada := a.client()
	ada.register("Ada", "hunter2hunter2")

	a.seedMovie(store.NewMovie{Title: "Levande Legend", Year: "1995", Rating: "7.4"})
	a.seedMovie(store.NewMovie{Title: "Bättre Betyg", Year: "1988", Rating: "8.6"})
	a.seedMovie(store.NewMovie{Title: "Nyare Film", Year: "2022", Rating: "8.6"})

	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// One vote each, so only the film itself separates them.
	ada.get("/")
	for _, m := range movies {
		ada.post("/movies/"+itoa64(m.ID)+"/vote", nil)
	}

	_, body := ada.get("/")
	hero := section(body, `class="hero-text"`, "</section>")
	mustContain(t, hero, "Nyare Film", "the newest of the best-rated films leads")
	for _, loser := range []string{"Bättre Betyg", "Levande Legend"} {
		if strings.Contains(hero, loser) {
			t.Errorf("%q should not be presented as the leader", loser)
		}
	}

	// And the podium is in the same order.
	podium := section(body, `<div class="podium">`, "</section>")
	first := strings.Index(podium, "Nyare Film")
	second := strings.Index(podium, "Bättre Betyg")
	third := strings.Index(podium, "Levande Legend")
	if first < 0 || second < 0 || third < 0 || !(first < second && second < third) {
		t.Errorf("podium order is wrong: newer=%d better=%d legend=%d", first, second, third)
	}
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

// Assets are served under a content-hashed URL, so a deploy cannot leave
// browsers running last version's CSS and JavaScript.
func TestAssetURLsAreVersioned(t *testing.T) {
	a := newApp(t, nil)
	c := a.client()
	c.register("Ada", "hunter2hunter2")

	_, body := c.get("/")
	matches := regexp.MustCompile(`/static/(style\.css|app\.js)\?v=([a-f0-9]{12})`).FindAllStringSubmatch(body, -1)
	if len(matches) < 2 {
		t.Fatalf("expected versioned css and js URLs, found %d", len(matches))
	}
	version := matches[0][2]
	for _, m := range matches {
		if m[2] != version {
			t.Errorf("assets carry different versions: %q and %q", version, m[2])
		}
	}

	// A versioned URL may be cached hard; an unversioned one must revalidate.
	resp, err := a.client().http.Get(a.ts.URL + "/static/app.js?v=" + version)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("versioned asset Cache-Control = %q, want it immutable", got)
	}

	plain, err := a.client().http.Get(a.ts.URL + "/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Body.Close()
	if got := plain.Header.Get("Cache-Control"); !strings.Contains(got, "must-revalidate") {
		t.Errorf("unversioned asset Cache-Control = %q, want revalidation", got)
	}
}
