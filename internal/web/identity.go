package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/o5ten/cinevote/internal/auth"
	"github.com/o5ten/cinevote/internal/mattermost"
	"github.com/o5ten/cinevote/internal/store"
)

// Mattermost mode: one shared password lets you in, and you say who you are by
// picking your chat account. This file is that half of the login — the account
// mode in handlers.go is untouched by it.
const (
	pendingCookie = "cinevote_pending"
	// pendingTTL is how long the gap between "password accepted" and "this is
	// who I am" may last. It is one page, so minutes are plenty.
	pendingTTL = 20 * time.Minute

	// memberCacheTTL is how long a directory listing is reused. The browser
	// asks for the whole list when the picker opens, and a chat server does not
	// gain members between two of those.
	memberCacheTTL = 5 * time.Minute
)

/* ------------------------------------------------------------ the password --- */

// handleSharedLogin takes the one password everybody shares. Which password it
// is decides the role; who you are comes next, from Mattermost.
func (s *Server) handleSharedLogin(w http.ResponseWriter, r *http.Request) {
	password := r.FormValue("password")

	if !s.logins.allow(clientIP(r)) {
		s.renderSharedLogin(w, r, http.StatusTooManyRequests,
			"För många försök. Vänta en stund och prova igen.")
		return
	}

	role := ""
	switch {
	case auth.SamePassword(password, s.cfg.AdminPassword):
		role = store.RoleAdmin
	case auth.SamePassword(password, s.cfg.SharedPassword):
	default:
		s.renderSharedLogin(w, r, http.StatusUnauthorized, "Fel lösenord.")
		return
	}
	s.logins.reset(clientIP(r))

	// There is no account yet, so the accepted password waits in a signed,
	// short-lived cookie until the person has picked who they are.
	http.SetCookie(w, &http.Cookie{
		Name:     pendingCookie,
		Value:    s.signer.Sign(role, pendingTTL),
		Path:     "/",
		MaxAge:   int(pendingTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/jagar", http.StatusSeeOther)
}

// pendingRole returns the role of a half-finished login, and whether there is
// one at all.
func (s *Server) pendingRole(r *http.Request) (string, bool) {
	c, err := r.Cookie(pendingCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	role, ok := s.signer.Verify(c.Value)
	if !ok {
		return "", false
	}
	if role != "" && role != store.RoleAdmin {
		return "", false
	}
	return role, true
}

func (s *Server) clearPending(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: pendingCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) renderSharedLogin(w http.ResponseWriter, r *http.Request, status int, problem string) {
	s.render(w, r, status, "login.html", map[string]any{
		"Title": "Logga in",
		"Error": problem,
	})
}

/* ------------------------------------------------------------- who am I? --- */

// handleIdentityForm asks which chat account the visitor is.
func (s *Server) handleIdentityForm(w http.ResponseWriter, r *http.Request) {
	role, ok := s.pendingRole(r)
	if !ok {
		// Either they never entered the password, or the gap timed out.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.renderIdentity(w, r, http.StatusOK, role, "", "")
}

// handleIdentitySave turns the picked account into a CineVote identity and
// opens the session for it.
func (s *Server) handleIdentitySave(w http.ResponseWriter, r *http.Request) {
	role, ok := s.pendingRole(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	typed := strings.TrimSpace(r.FormValue("member"))
	if typed == "" {
		s.renderIdentity(w, r, http.StatusBadRequest, role, typed, "Välj vem du är i listan.")
		return
	}

	user, problem := s.findMember(r.Context(), typed)
	if problem != "" {
		s.renderIdentity(w, r, http.StatusUnprocessableEntity, role, typed, problem)
		return
	}

	account, err := s.store.UpsertMattermostUser(r.Context(),
		user.Username, user.ID, user.DisplayName())
	if err != nil {
		s.fail(w, r, "kunde inte koppla ditt konto", err)
		return
	}

	if err := s.startSessionAs(w, r, account, role); err != nil {
		s.fail(w, r, "kunde inte logga in", err)
		return
	}
	s.clearPending(w)
	s.log.Info("identity chosen", "mattermost", account.MMUsername, "role", role)
	s.setFlash(w, "ok", "Välkommen, "+account.Name()+"!")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleSwitchIdentity drops the session but keeps the password, so a shared
// screen can hand over to the next person without knowing it again.
func (s *Server) handleSwitchIdentity(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	role := ""
	if user != nil && user.IsAdmin {
		role = store.RoleAdmin
	}

	s.endSession(w, r)
	http.SetCookie(w, &http.Cookie{
		Name:     pendingCookie,
		Value:    s.signer.Sign(role, pendingTTL),
		Path:     "/",
		MaxAge:   int(pendingTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/jagar", http.StatusSeeOther)
}

func (s *Server) renderIdentity(w http.ResponseWriter, r *http.Request, status int, role, typed, problem string) {
	s.render(w, r, status, "identity.html", map[string]any{
		"Title":     "Vem är du?",
		"Error":     problem,
		"Typed":     typed,
		"AsAdmin":   role == store.RoleAdmin,
		"ChatURL":   s.mm.BaseURL(),
		"ChatOn":    s.mm.Enabled(),
		"PendingOK": true,
	})
}

/* --------------------------------------------------------- the directory --- */

// memberSuggestion is one row in the picker. Name is what the reader sees,
// Username is what the form submits.
type memberSuggestion struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

// memberList is what the picker gets to work with.
//
// AskServer tells the browser not to rely on Users and to let this server
// search instead. Two things ask for that: a directory too large to send at
// once, and a token allowed to search but not to list.
//
// Unreachable says the lookup failed rather than found nobody — an empty list
// and a broken one look identical to a browser, and the difference is the whole
// message.
type memberList struct {
	Users       []memberSuggestion `json:"users"`
	AskServer   bool               `json:"askServer"`
	Unreachable bool               `json:"unreachable"`
}

// memberCache holds the directory between requests.
type memberCache struct {
	mu   sync.Mutex
	list memberList
	at   time.Time
}

// handleMembers answers the picker. Without ?q= it returns everybody, which
// the browser indexes and filters as you type; with ?q= it searches on the
// server, the fallback for a directory too large to send.
//
// It is open to anyone holding a half-finished login, because that is exactly
// who needs it — but no further: the list of who is in the chat should not be
// readable without the shared password.
func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request) {
	var (
		out memberList
		err error
	)
	if term := strings.TrimSpace(r.URL.Query().Get("q")); term != "" {
		out, err = s.searchMembers(r.Context(), term)
	} else {
		out, err = s.memberDirectory(r.Context())
	}
	if err != nil {
		// Saying so lets the picker fall back to asking us to search, and show
		// a reason if that fails too. Offering nothing looks like a field that
		// simply does not work.
		s.log.Error("mattermost lookup", "q", r.URL.Query().Get("q"), "err", err)
		out = memberList{Users: []memberSuggestion{}, AskServer: true, Unreachable: true}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.log.Error("encode member list", "err", err)
	}
}

func (s *Server) searchMembers(ctx context.Context, term string) (memberList, error) {
	out := memberList{Users: []memberSuggestion{}}
	if len([]rune(term)) < 2 {
		return out, nil
	}
	users, err := s.mm.Search(ctx, term)
	if err != nil {
		return out, err
	}
	out.Users = suggestions(users)
	return out, nil
}

func (s *Server) memberDirectory(ctx context.Context) (memberList, error) {
	s.members.mu.Lock()
	defer s.members.mu.Unlock()

	if !s.members.at.IsZero() && time.Since(s.members.at) < memberCacheTTL {
		return s.members.list, nil
	}
	users, truncated, err := s.mm.Directory(ctx)
	if err != nil {
		// Not cached: the next keystroke should be free to try again.
		return memberList{Users: []memberSuggestion{}}, err
	}
	if len(users) == 0 {
		// A chat server with nobody in it is not a real state; a listing that
		// comes back empty means the token may not read the directory, or the
		// paging did something unexpected. Either way, let the browser ask us
		// to search instead of leaving it with nothing — and do not cache it,
		// so the next attempt is free to succeed.
		return memberList{Users: []memberSuggestion{}, AskServer: true}, nil
	}
	if truncated {
		s.log.Warn("the mattermost directory is larger than the picker holds; "+
			"falling back to searching on the server",
			"listed", len(users), "limit", mattermost.DirectoryLimit)
	}

	list := memberList{Users: suggestions(users), AskServer: truncated}
	s.members.list, s.members.at = list, time.Now()
	return list, nil
}

// suggestions sorts accounts by the name the reader reads, so the list is in a
// sensible order before anybody types.
func suggestions(users []mattermost.User) []memberSuggestion {
	out := make([]memberSuggestion, 0, len(users))
	for _, u := range users {
		if !u.Active() {
			continue
		}
		out = append(out, memberSuggestion{Username: u.Username, Name: u.DisplayName()})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := mattermost.Fold(out[i].Name), mattermost.Fold(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Username < out[j].Username
	})
	return out
}

// findMember turns what was submitted into one account, or into the sentence
// the person should read. The field is a text input, so it has to cope with
// whatever is left in it: a username picked from the list, "@anna" pasted from
// a message, or a full name typed by hand. Anything pointing at exactly one
// person resolves; anything pointing at several says who they are, so one more
// keystroke settles it.
func (s *Server) findMember(ctx context.Context, typed string) (mattermost.User, string) {
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return mattermost.User{}, "Välj vem du är i listan."
	}

	// A username is an exact address: look it up and skip searching.
	if username := mattermost.Username(typed); mattermost.LooksLikeUsername(username) {
		if u, err := s.mm.ByUsername(ctx, username); err == nil {
			return u, ""
		}
		// Not a username after all — fall through and search it as a name, so
		// "Bo" finds Bo even though it looked like one.
	}

	candidates, err := s.mm.Search(ctx, typed)
	if err != nil {
		s.log.Error("mattermost name search", "term", typed, "err", err)
		return mattermost.User{}, "Kunde inte nå Mattermost just nu. Prova igen om en stund."
	}

	// A term that is somebody's whole name or username beats one that merely
	// starts it: with both "Anna Andersson" and "Anna Anderssen" in the chat,
	// typing the first name in full means the first person.
	if exact := exactly(candidates, typed); len(exact) > 0 {
		candidates = exact
	}

	switch len(candidates) {
	case 1:
		return candidates[0], ""
	case 0:
		return mattermost.User{}, "Hittade ingen som heter " + typed + " i Mattermost."
	default:
		// Never guess between people. Naming them turns a dead end into a
		// choice.
		return mattermost.User{}, "Flera personer matchar " + typed + ": " + describe(candidates) +
			". Skriv lite mer, eller välj i listan."
	}
}

// exactly returns the accounts whose name, nickname or username is the term,
// give or take capitals and accents.
func exactly(users []mattermost.User, term string) []mattermost.User {
	want := mattermost.Fold(term)
	var out []mattermost.User
	for _, u := range users {
		switch want {
		case mattermost.Fold(u.DisplayName()), mattermost.Fold(u.Nickname), mattermost.Fold(u.Username):
			out = append(out, u)
		}
	}
	return out
}

// describe lists people the way the pages talk about them.
func describe(users []mattermost.User) string {
	const most = 5
	var names []string
	for i, u := range users {
		if i == most {
			names = append(names, "med flera")
			break
		}
		names = append(names, u.DisplayName()+" (@"+u.Username+")")
	}
	return strings.Join(names, ", ")
}
