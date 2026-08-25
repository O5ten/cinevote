// Package mattermost reads the user directory of a Mattermost server, so a
// person can say who they are by picking their own account instead of creating
// yet another login. It is the same integration the sibling sites (dinner,
// booking) use, cut down to what a voting board needs: looking people up. It
// never posts anything.
//
// With no server or token configured the client is disabled and every lookup
// answers empty, which leaves CineVote on its own accounts.
package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// timeout bounds a single API call. The bot sits on the same network as the
// site, so anything slower than this is broken rather than busy.
const timeout = 15 * time.Second

// searchLimit caps how many people one directory search returns.
const searchLimit = 20

const (
	// directoryPage is how many accounts one listing request asks for. 200 is
	// the largest page Mattermost serves.
	directoryPage = 200
	// DirectoryLimit caps a full listing. A group of friends is tens of
	// people, not thousands; the cap stops us paging through a large company
	// server forever, and callers are told when it bites.
	DirectoryLimit = 1000
)

// User is the part of a Mattermost account this site cares about.
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Nickname  string `json:"nickname"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	IsBot     bool   `json:"is_bot"`
	DeleteAt  int64  `json:"delete_at"`
}

// Active reports whether the account is a real, non-deactivated person.
func (u User) Active() bool { return u.ID != "" && u.DeleteAt == 0 && !u.IsBot }

// DisplayName is the name to show: real name when the account has one, then
// the nickname, then the username.
func (u User) DisplayName() string {
	full := strings.TrimSpace(u.FirstName + " " + u.LastName)
	switch {
	case full != "":
		return full
	case u.Nickname != "":
		return u.Nickname
	default:
		return u.Username
	}
}

// Client is the site's read-only connection to Mattermost.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	self    User // filled by Verify, so a bad token fails loudly at startup
}

// New builds a client. An empty url or token yields a disabled one.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: timeout},
	}
}

// Enabled reports whether calls will really reach a Mattermost server.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" && c.token != "" }

// BaseURL is the server address, for links back into Mattermost.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// Bot returns the account the token belongs to, once Verify has run.
func (c *Client) Bot() User {
	if c == nil {
		return User{}
	}
	return c.self
}

// Verify checks the token and remembers whose it is. Run it at startup so a
// bad token is a loud failure rather than a mystery at the login page.
func (c *Client) Verify(ctx context.Context) (User, error) {
	if !c.Enabled() {
		return User{}, nil
	}
	var u User
	if err := c.call(ctx, http.MethodGet, "/api/v4/users/me", nil, &u); err != nil {
		return User{}, err
	}
	c.self = u
	return u, nil
}

// Search asks Mattermost to find people matching a term. Used when the
// directory is too large to hold in the browser, or when the token may search
// but not list.
func (c *Client) Search(ctx context.Context, term string) ([]User, error) {
	term = strings.TrimSpace(term)
	if !c.Enabled() || term == "" {
		return nil, nil
	}
	body := map[string]any{
		"term":           term,
		"allow_inactive": false,
		"limit":          searchLimit,
	}
	var users []User
	if err := c.call(ctx, http.MethodPost, "/api/v4/users/search", body, &users); err != nil {
		return nil, err
	}
	return active(users), nil
}

// Directory lists everybody, so one request can fill a picker the browser then
// searches without a round trip per keystroke. The bool reports that the
// listing was cut short at DirectoryLimit, which means the caller is looking at
// part of a much larger server and should let this server search instead.
func (c *Client) Directory(ctx context.Context) ([]User, bool, error) {
	if !c.Enabled() {
		return nil, false, nil
	}
	var out []User
	for page := 0; ; page++ {
		var batch []User
		path := fmt.Sprintf("/api/v4/users?active=true&per_page=%d&page=%d", directoryPage, page)
		if err := c.call(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, false, err
		}
		out = append(out, active(batch)...)
		if len(batch) < directoryPage {
			return out, false, nil
		}
		if len(out) >= DirectoryLimit {
			return out[:DirectoryLimit], true, nil
		}
	}
}

// ByUsername looks up exactly one account.
func (c *Client) ByUsername(ctx context.Context, username string) (User, error) {
	username = Username(username)
	if username == "" {
		return User{}, fmt.Errorf("empty username")
	}
	if !c.Enabled() {
		return User{Username: username}, nil
	}
	var u User
	if err := c.call(ctx, http.MethodGet, "/api/v4/users/username/"+url.PathEscape(username), nil, &u); err != nil {
		return User{}, err
	}
	if !u.Active() {
		return User{}, fmt.Errorf("mattermost user %q is not an active person", username)
	}
	return u, nil
}

func active(users []User) []User {
	out := make([]User, 0, len(users))
	for _, u := range users {
		if u.Active() {
			out = append(out, u)
		}
	}
	return out
}

// call performs one API request, encoding body and decoding into out when they
// are non-nil.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mattermost %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body) // drain so the connection is reusable
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// apiError turns Mattermost's error body into something readable in a log.
func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return fmt.Errorf("mattermost %s: %s", resp.Status, e.Message)
	}
	return fmt.Errorf("mattermost %s", resp.Status)
}

// folder reduces the accented letters that turn up in these names to their
// plain forms. Deliberately a short table rather than full Unicode
// normalization: these are the letters people actually type differently.
var folder = strings.NewReplacer(
	"å", "a", "ä", "a", "á", "a", "à", "a", "â", "a", "ã", "a",
	"ö", "o", "ø", "o", "ó", "o", "ò", "o", "ô", "o", "õ", "o",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ú", "u", "ù", "u", "û", "u", "ü", "u",
	"ý", "y", "ÿ", "y", "ñ", "n", "ç", "c", "ð", "d", "þ", "th", "ß", "ss",
	"æ", "ae", "œ", "oe", "ł", "l", "š", "s", "ž", "z", "č", "c", "ř", "r",
)

// Fold turns a name into the form searches compare: lowercase, without
// accents, so "Östberg" and "ostberg" find each other. The browser folds the
// same way, so both ends agree on what matches.
func Fold(s string) string {
	return folder.Replace(strings.ToLower(strings.TrimSpace(s)))
}

// Username accepts what a person is likely to type — "@anna", "Anna", or a
// pasted profile link — and returns "anna".
func Username(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "@")))
}

// LooksLikeUsername reports whether a value could be a Mattermost username at
// all. Names with spaces or accents never are.
func LooksLikeUsername(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
