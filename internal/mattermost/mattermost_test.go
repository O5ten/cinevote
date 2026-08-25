package mattermost_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/o5ten/cinevote/internal/mattermost"
	"github.com/o5ten/cinevote/internal/mattermost/mmtest"
)

/* --------------------------------------------------------------- the tests --- */

func people() []mattermost.User {
	return []mattermost.User{
		mattermost.User{ID: "u1", Username: "anna.andersson", FirstName: "Anna", LastName: "Andersson"},
		mattermost.User{ID: "u2", Username: "bjorn", FirstName: "Björn", LastName: "Östberg"},
		mattermost.User{ID: "u3", Username: "cissi", Nickname: "Cissi"},
		mattermost.User{ID: "u4", Username: "gamla.kontot", FirstName: "Gamla", DeleteAt: 1700000000},
		mattermost.User{ID: "u5", Username: "annan.bot", FirstName: "Bot", IsBot: true},
	}
}

func TestDisabledClientDoesNothing(t *testing.T) {
	c := mattermost.New("", "")
	if c.Enabled() {
		t.Fatal("a client with no url or token should be disabled")
	}
	if users, err := c.Search(context.Background(), "anna"); err != nil || users != nil {
		t.Errorf("Search = %v, %v; want nothing", users, err)
	}
	if users, truncated, err := c.Directory(context.Background()); err != nil || users != nil || truncated {
		t.Errorf("Directory = %v, %v, %v; want nothing", users, truncated, err)
	}
	// ByUsername still echoes the name, so a caller can carry on without chat.
	u, err := c.ByUsername(context.Background(), "@Anna")
	if err != nil || u.Username != "anna" {
		t.Errorf("ByUsername = %+v, %v", u, err)
	}
}

func TestHalfConfiguredIsDisabled(t *testing.T) {
	if mattermost.New("https://chat.example.se", "").Enabled() {
		t.Error("a url without a token should not count as configured")
	}
	if mattermost.New("", "token").Enabled() {
		t.Error("a token without a url should not count as configured")
	}
}

func TestVerifyIdentifiesTheToken(t *testing.T) {
	c := mmtest.New(t, people()...).Client()
	bot, err := c.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bot.Username != "cinevote-bot" || c.Bot().Username != bot.Username {
		t.Fatalf("bot = %+v", bot)
	}
}

func TestVerifyRejectsABadToken(t *testing.T) {
	fake := mmtest.New(t, people()...)
	c := mattermost.New(fake.URL, "wrong-token")
	if _, err := c.Verify(context.Background()); err == nil {
		t.Fatal("a bad token should fail at startup, not silently later")
	}
}

func TestDirectorySkipsBotsAndDeactivatedAccounts(t *testing.T) {
	c := mmtest.New(t, people()...).Client()
	users, truncated, err := c.Directory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("a five-person server should not truncate")
	}
	if len(users) != 3 {
		t.Fatalf("got %d people, want the 3 real ones: %+v", len(users), users)
	}
	for _, u := range users {
		if u.IsBot || u.DeleteAt != 0 {
			t.Errorf("%s should have been filtered out", u.Username)
		}
	}
}

func TestDirectoryPagesAndTruncates(t *testing.T) {
	var many []mattermost.User
	for i := 0; i < mattermost.DirectoryLimit+50; i++ {
		many = append(many, mattermost.User{
			ID:       fmt.Sprintf("u%d", i),
			Username: fmt.Sprintf("person%d", i),
		})
	}
	c := mmtest.New(t, many...).Client()

	users, truncated, err := c.Directory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("a directory past the limit should say it was cut short")
	}
	if len(users) != mattermost.DirectoryLimit {
		t.Fatalf("got %d people, want the %d-person cap", len(users), mattermost.DirectoryLimit)
	}
}

func TestSearchFindsPeopleByNameAndUsername(t *testing.T) {
	c := mmtest.New(t, people()...).Client()
	ctx := context.Background()

	for _, term := range []string{"anna", "Andersson", "ostberg", "Östberg", "cissi"} {
		users, err := c.Search(ctx, term)
		if err != nil {
			t.Fatalf("search %q: %v", term, err)
		}
		if len(users) == 0 {
			t.Errorf("searching %q found nobody", term)
		}
	}
	if users, err := c.Search(ctx, "ingen-sådan"); err != nil || len(users) != 0 {
		t.Errorf("search for a stranger = %v, %v", users, err)
	}
}

func TestByUsername(t *testing.T) {
	c := mmtest.New(t, people()...).Client()
	ctx := context.Background()

	u, err := c.ByUsername(ctx, "@anna.andersson")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "u1" || u.DisplayName() != "Anna Andersson" {
		t.Fatalf("got %+v", u)
	}
	if _, err := c.ByUsername(ctx, "nobody"); err == nil {
		t.Error("an unknown username should be an error")
	}
	// A deactivated account is not a person you can be.
	if _, err := c.ByUsername(ctx, "gamla.kontot"); err == nil {
		t.Error("a deactivated account should be refused")
	}
}

func TestUnreachableServerIsAnError(t *testing.T) {
	fake := mmtest.New(t, people()...)
	fake.Deny = true
	c := fake.Client()

	if _, err := c.Search(context.Background(), "anna"); err == nil {
		t.Error("a failing server should be reported, not read as an empty result")
	}
	if _, _, err := c.Directory(context.Background()); err == nil {
		t.Error("a failing listing should be reported")
	}
}

func TestDisplayName(t *testing.T) {
	cases := []struct {
		user mattermost.User
		want string
	}{
		{mattermost.User{Username: "anna", FirstName: "Anna", LastName: "Andersson"}, "Anna Andersson"},
		{mattermost.User{Username: "anna", FirstName: "Anna"}, "Anna"},
		{mattermost.User{Username: "anna", Nickname: "Ankan"}, "Ankan"},
		{mattermost.User{Username: "anna"}, "anna"},
	}
	for _, tc := range cases {
		if got := tc.user.DisplayName(); got != tc.want {
			t.Errorf("DisplayName(%+v) = %q, want %q", tc.user, got, tc.want)
		}
	}
}

func TestUsernameNormalisation(t *testing.T) {
	cases := map[string]string{
		"anna":                             "anna",
		"@anna":                            "anna",
		"  @Anna  ":                        "anna",
		"https://chat.example.se/team/@bo": "bo",
		"https://chat.example.se/team/bo":  "bo",
		"":                                 "",
	}
	for in, want := range cases {
		if got := mattermost.Username(in); got != want {
			t.Errorf("Username(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooksLikeUsername(t *testing.T) {
	for _, ok := range []string{"anna", "anna.andersson", "bo-42", "x_y"} {
		if !mattermost.LooksLikeUsername(ok) {
			t.Errorf("%q should look like a username", ok)
		}
	}
	for _, notOk := range []string{"", "Anna Andersson", "björn", "anna@example.se"} {
		if mattermost.LooksLikeUsername(notOk) {
			t.Errorf("%q should not look like a username", notOk)
		}
	}
}

func TestFold(t *testing.T) {
	if mattermost.Fold(" Östberg ") != "ostberg" {
		t.Errorf("Fold = %q", mattermost.Fold(" Östberg "))
	}
	if mattermost.Fold("Anna") != mattermost.Fold("anna") {
		t.Error("folding should ignore case")
	}
}
