package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/o5ten/cinevote/internal/config"
	"github.com/o5ten/cinevote/internal/mattermost"
	"github.com/o5ten/cinevote/internal/mattermost/mmtest"
)

// chatApp is CineVote in Mattermost mode, against a fake chat server.
func chatApp(t *testing.T, tune func(*mmtest.Fake)) (*app, *mmtest.Fake) {
	t.Helper()
	fake := mmtest.New(t,
		mattermost.User{ID: "u1", Username: "anna.andersson", FirstName: "Anna", LastName: "Andersson"},
		mattermost.User{ID: "u2", Username: "bjorn", FirstName: "Björn", LastName: "Östberg"},
		mattermost.User{ID: "u3", Username: "cissi", Nickname: "Cissi"},
		mattermost.User{ID: "u4", Username: "anna.svensson", FirstName: "Anna", LastName: "Svensson"},
	)
	if tune != nil {
		tune(fake)
	}
	a := newAppWithChat(t, func(c *config.Config) {
		c.Mattermost = config.MattermostSettings{URL: fake.URL, Token: "test-token"}
		c.SharedPassword = "filmkvall"
		c.AdminPassword = "chefen-bestammer"
	}, fake.Client())
	return a, fake
}

// signIn gives the shared password and picks an identity.
func (c *client) signIn(password, member string) (int, string) {
	c.t.Helper()
	c.get("/login")
	status, body := c.post("/login", url.Values{"password": {password}})
	if status != http.StatusOK {
		return status, body
	}
	return c.post("/jagar", url.Values{"member": {member}})
}

func TestMattermostModeLoginIsPasswordOnly(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()

	_, body := c.get("/login")
	if strings.Contains(body, `name="username"`) {
		t.Error("Mattermost mode should not ask for a username")
	}
	mustContain(t, body, `name="password"`, "password field")
	mustContain(t, body, "sen väljer du vem du är", "explanation")
	if strings.Contains(body, "/register") {
		t.Error("there is nothing to register when identity comes from the chat")
	}

	// Registration is gone, not merely hidden.
	status, _ := c.get("/register")
	if status != http.StatusNotFound {
		t.Errorf("GET /register = %d, want 404 in Mattermost mode", status)
	}
}

func TestSharedPasswordThenPickWhoYouAre(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()

	// The password alone lands on the identity page, not the board.
	c.get("/login")
	status, body := c.post("/login", url.Values{"password": {"filmkvall"}})
	if status != http.StatusOK {
		t.Fatalf("login status %d", status)
	}
	mustContain(t, body, "Vem är du?", "identity page")

	// Anything else redirects there too until the question is answered.
	_, body = c.get("/")
	mustContain(t, body, "Vem är du?", "the board before an identity is chosen")

	status, body = c.post("/jagar", url.Values{"member": {"anna.andersson"}})
	if status != http.StatusOK {
		t.Fatalf("identity status %d: %s", status, first(body, 300))
	}
	mustContain(t, body, "Välkommen, Anna Andersson", "greeting by the chat name")
	mustContain(t, body, "Nästa filmkväll", "the board")

	// The account exists, tied to the chat account and with no password.
	user, err := a.store.UserByMattermost(context.Background(), "anna.andersson")
	if err != nil {
		t.Fatal(err)
	}
	if user.MMUserID != "u1" || user.DisplayName != "Anna Andersson" {
		t.Fatalf("account = %+v", user)
	}
	if user.PasswordHash != "" {
		t.Error("a chat-identified account should have no password of its own")
	}
}

func TestWrongSharedPasswordIsRefused(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()

	c.get("/login")
	status, body := c.post("/login", url.Values{"password": {"gissning"}})
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	mustContain(t, body, "Fel lösenord", "rejection")

	// And no half-finished login was handed out.
	_, body = c.get("/")
	mustContain(t, body, "Logga in", "still at the door")
}

func TestIdentityPageNeedsThePasswordFirst(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()

	_, body := c.get("/jagar")
	mustContain(t, body, "Logga in", "identity page without a password")

	// The directory is not readable either.
	status, _ := c.get("/medlemmar")
	if status != http.StatusUnauthorized {
		t.Errorf("GET /medlemmar = %d, want 401 without the password", status)
	}
}

func TestDirectoryIsOfferedToThePicker(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()
	c.get("/login")
	c.post("/login", url.Values{"password": {"filmkvall"}})

	status, body := c.get("/medlemmar")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{"anna.andersson", "Anna Andersson", "Björn Östberg", "Cissi"} {
		mustContain(t, body, want, "directory")
	}
	mustContain(t, body, `"askServer":false`, "the browser can filter this itself")

	// Server-side search is there for the directories that are too big.
	_, body = c.get("/medlemmar?q=ostberg")
	mustContain(t, body, "bjorn", "folded search")
	if strings.Contains(body, "anna.andersson") {
		t.Error("the search should narrow, not return everybody")
	}
}

// A token that may search but not list must still leave a working picker.
func TestPickerFallsBackToServerSearch(t *testing.T) {
	a, _ := chatApp(t, func(f *mmtest.Fake) { f.NoList = true })
	c := a.client()
	c.get("/login")
	c.post("/login", url.Values{"password": {"filmkvall"}})

	_, body := c.get("/medlemmar")
	mustContain(t, body, `"askServer":true`, "ask the server instead")
	mustContain(t, body, `"unreachable":true`, "and say the listing failed")

	// Searching still works, so somebody can still sign in.
	_, body = c.get("/medlemmar?q=anna")
	mustContain(t, body, "anna.andersson", "search still works")
}

func TestIdentityAcceptsANameAndAPastedHandle(t *testing.T) {
	a, _ := chatApp(t, nil)

	for _, typed := range []string{"anna.andersson", "@anna.andersson", "Anna Andersson", "Björn Östberg", "Cissi"} {
		c := a.client()
		status, body := c.signIn("filmkvall", typed)
		if status != http.StatusOK {
			t.Errorf("%q: status %d", typed, status)
			continue
		}
		if !strings.Contains(body, "Välkommen") {
			t.Errorf("%q did not resolve to a person: %s", typed, first(body, 200))
		}
	}
}

func TestAmbiguousIdentityAsksAgain(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()
	c.get("/login")
	c.post("/login", url.Values{"password": {"filmkvall"}})

	// Two Annas: never guess.
	status, body := c.post("/jagar", url.Values{"member": {"Anna"}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", status)
	}
	mustContain(t, body, "Flera personer matchar", "ambiguity")
	mustContain(t, body, "Anna Andersson (@anna.andersson)", "who they are")
	mustContain(t, body, "Anna Svensson (@anna.svensson)", "who they are")
}

func TestUnknownIdentityIsRefused(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()
	c.get("/login")
	c.post("/login", url.Values{"password": {"filmkvall"}})

	status, body := c.post("/jagar", url.Values{"member": {"Ingen Alls"}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", status)
	}
	mustContain(t, body, "Hittade ingen", "rejection")
}

// The admin password grants admin rights to whoever signs in with it, since
// there are no accounts to carry the flag.
func TestAdminPasswordGrantsAdminInMattermostMode(t *testing.T) {
	a, _ := chatApp(t, nil)

	member := a.client()
	if status, _ := member.signIn("filmkvall", "cissi"); status != http.StatusOK {
		t.Fatalf("member sign-in failed: %d", status)
	}
	if status, _ := member.get("/admin"); status != http.StatusForbidden {
		t.Errorf("an ordinary member reached /admin: %d", status)
	}

	admin := a.client()
	admin.get("/login")
	status, body := admin.post("/login", url.Values{"password": {"chefen-bestammer"}})
	if status != http.StatusOK {
		t.Fatalf("admin password refused: %d", status)
	}
	mustContain(t, body, "adminlösenordet", "the identity page says what the password unlocked")

	status, body = admin.post("/jagar", url.Values{"member": {"bjorn"}})
	if status != http.StatusOK {
		t.Fatalf("admin sign-in failed: %d", status)
	}

	status, body = admin.get("/admin")
	if status != http.StatusOK {
		t.Fatalf("admin page = %d", status)
	}
	mustContain(t, body, "Filmer", "admin page")
}

// Voting works the same as ever, and names people by their chat name.
func TestVotingUnderAMattermostIdentity(t *testing.T) {
	a, _ := chatApp(t, nil)

	anna := a.client()
	if status, _ := anna.signIn("filmkvall", "anna.andersson"); status != http.StatusOK {
		t.Fatal("sign-in failed")
	}
	anna.addMovie("Stalker")

	movies, err := a.store.Movies(context.Background(), 0)
	if err != nil || len(movies) != 1 {
		t.Fatalf("expected one movie: %v %v", movies, err)
	}
	_, body := anna.post("/movies/"+itoa64(movies[0].ID)+"/vote", nil)
	mustContain(t, body, "Föreslagen av Anna Andersson", "the chat name, not the handle")
	mustContain(t, body, "4 / 5 röster kvar", "the vote counted")
}

// Two people signing in as the same chat account are the same person, not two.
func TestSameChatAccountIsOneMember(t *testing.T) {
	a, _ := chatApp(t, nil)

	first := a.client()
	first.signIn("filmkvall", "cissi")
	second := a.client()
	second.signIn("filmkvall", "cissi")

	users, err := a.store.UserStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d accounts, want 1: %+v", len(users), users)
	}
}

// A shared screen hands over without the password being typed again.
func TestSwitchUserKeepsThePassword(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()
	c.signIn("filmkvall", "cissi")

	_, body := c.get("/")
	mustContain(t, body, "Byt användare", "the hand-over button")

	status, body := c.post("/byt-anvandare", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mustContain(t, body, "Vem är du?", "back to the picker, not the password")

	// And the next person can just pick themselves.
	status, body = c.post("/jagar", url.Values{"member": {"bjorn"}})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mustContain(t, body, "Välkommen, Björn Östberg", "the next person")
}

// Logging out really is out: the password is needed again.
func TestLogoutClearsTheHalfFinishedLoginToo(t *testing.T) {
	a, _ := chatApp(t, nil)
	c := a.client()
	c.signIn("filmkvall", "cissi")

	c.post("/logout", nil)
	_, body := c.get("/")
	mustContain(t, body, "Logga in", "back to the password")
	if strings.Contains(body, "Vem är du?") {
		t.Error("logging out should not leave a usable half-finished login")
	}
}

// Account mode must be untouched by any of this.
func TestAccountModeStillWorks(t *testing.T) {
	a := newApp(t, nil)
	c := a.client()

	_, body := c.get("/login")
	mustContain(t, body, `name="username"`, "account login")
	mustContain(t, body, "/register", "sign-up link")
	if strings.Contains(body, "sen väljer du vem du är") {
		t.Error("account mode should not talk about picking an identity")
	}

	c.register("Ada", "hunter2hunter2")
	_, body = c.get("/")
	mustContain(t, body, "Ada", "signed in")
	if strings.Contains(body, "Byt användare") {
		t.Error("there is nobody to switch to without a chat directory")
	}
	if status, _ := c.get("/medlemmar"); status != http.StatusNotFound {
		t.Error("the directory endpoint should not exist in account mode")
	}
}

// An empty listing is not a real state: it means the token cannot read the
// directory. Leaving the picker with nothing would look like a broken field, so
// it falls back to searching — and the empty answer must not be cached.
func TestEmptyDirectoryFallsBackToSearching(t *testing.T) {
	a, fake := chatApp(t, func(f *mmtest.Fake) { f.Users = nil })
	c := a.client()
	c.get("/login")
	c.post("/login", url.Values{"password": {"filmkvall"}})

	_, body := c.get("/medlemmar")
	mustContain(t, body, `"askServer":true`, "fall back to searching")

	// Nothing was cached, so people appearing works on the next request.
	fake.Users = []mattermost.User{
		{ID: "u9", Username: "nykomling", FirstName: "Ny", LastName: "Komling"},
	}
	_, body = c.get("/medlemmar")
	mustContain(t, body, "nykomling", "the next listing is tried afresh")
	mustContain(t, body, `"askServer":false`, "and can be filtered in the browser")
}
