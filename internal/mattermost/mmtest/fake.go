// Package mmtest is a fake Mattermost server for tests. It lives apart from
// the mattermost package so that nothing in the shipped binary links
// net/http/httptest, which registers a command-line flag of its own.
package mmtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/o5ten/cinevote/internal/mattermost"
)

// Fake is a stand-in Mattermost server, also used by the web tests.
type Fake struct {
	*httptest.Server
	Users []mattermost.User
	// Deny makes every call fail, for the unreachable-server case.
	Deny bool
	// NoList makes listing fail while search keeps working, which is what a
	// token without directory rights looks like.
	NoList bool
	Calls  int
}

// New starts a fake server with the given accounts.
func New(t *testing.T, users ...mattermost.User) *Fake {
	t.Helper()
	f := &Fake{Users: users}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Client returns a client pointed at the fake.
func (f *Fake) Client() *mattermost.Client { return mattermost.New(f.URL, "test-token") }

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.Calls++
	if f.Deny {
		http.Error(w, `{"message":"nope"}`, http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, `{"message":"invalid token"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/api/v4/users/me":
		json.NewEncoder(w).Encode(mattermost.User{ID: "bot", Username: "cinevote-bot", IsBot: true})

	case r.URL.Path == "/api/v4/users/search":
		var body struct {
			Term string `json:"term"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(f.match(body.Term))

	case r.URL.Path == "/api/v4/users":
		if f.NoList {
			http.Error(w, `{"message":"you do not have the appropriate permissions"}`, http.StatusForbidden)
			return
		}
		// Page the way the real server does, so paging is exercised.
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if per <= 0 {
			per = 200
		}
		from := page * per
		if from >= len(f.Users) {
			json.NewEncoder(w).Encode([]mattermost.User{})
			return
		}
		to := from + per
		if to > len(f.Users) {
			to = len(f.Users)
		}
		json.NewEncoder(w).Encode(f.Users[from:to])

	case strings.HasPrefix(r.URL.Path, "/api/v4/users/username/"):
		want := strings.TrimPrefix(r.URL.Path, "/api/v4/users/username/")
		for _, u := range f.Users {
			if u.Username == want {
				json.NewEncoder(w).Encode(u)
				return
			}
		}
		http.Error(w, `{"message":"Unable to find the user."}`, http.StatusNotFound)

	default:
		http.Error(w, fmt.Sprintf(`{"message":"unexpected path %s"}`, r.URL.Path), http.StatusNotFound)
	}
}

// match is a rough stand-in for Mattermost's own search.
func (f *Fake) match(term string) []mattermost.User {
	needle := mattermost.Fold(term)
	var out []mattermost.User
	for _, u := range f.Users {
		hay := mattermost.Fold(u.DisplayName() + " " + u.Username + " " + u.Nickname)
		if needle == "" || strings.Contains(hay, needle) {
			out = append(out, u)
		}
	}
	return out
}
