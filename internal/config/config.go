// Package config reads the runtime configuration from the environment so the
// same binary works unchanged in a container.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MattermostSettings points at the chat server whose accounts identify people.
// Both fields or neither: half a configuration looks connected and is not.
type MattermostSettings struct {
	URL   string // e.g. "https://chat.example.se"
	Token string // a bot or personal access token that may read the directory
}

// Enabled reports whether lookups will really reach a Mattermost server.
func (m MattermostSettings) Enabled() bool { return m.URL != "" && m.Token != "" }

type Config struct {
	Addr             string        // listen address, e.g. ":8080"
	DBPath           string        // SQLite file
	SiteName         string        // shown in the header
	AdminUsername    string        // the one admin account
	AdminPassword    string        // bootstrapped at startup; random if unset
	RegistrationCode string        // optional shared invite code
	PosterSource     string        // "imdb" (OMDb), "tmdb", "none" or "" for auto
	OMDbAPIKey       string        // omdbapi.com key, enables IMDb lookups
	TMDBAPIKey       string        // themoviedb.org key, alternative backend
	MaxVotes         int           // votes per user
	SessionTTL       time.Duration // login lifetime
	SecureCookies    bool          // set when served over HTTPS

	// Mattermost identifies people instead of CineVote's own accounts. When
	// it is configured, one shared password lets you in and you say who you
	// are by picking your chat account — the same way the sibling sites work.
	Mattermost MattermostSettings
	// SharedPassword is the one password everybody uses in Mattermost mode.
	SharedPassword string

	// Demo mode: seed a populated database and show the logins in the UI.
	// Nothing needs to be configured for it, so it is also the mode that must
	// never be reachable by accident — only an explicit -demo flag or
	// CINEVOTE_DEMO=true turns it on.
	Demo bool
	// FreshDB asks the caller to delete the database before opening it, so a
	// demo always starts from the same state.
	FreshDB bool
}

func Load() (Config, error) {
	c := Config{
		Addr:             env("CINEVOTE_ADDR", ":8080"),
		DBPath:           env("CINEVOTE_DB", DefaultDBPath),
		SiteName:         env("CINEVOTE_SITE_NAME", "CineVote"),
		AdminUsername:    env("CINEVOTE_ADMIN_USERNAME", "admin"),
		AdminPassword:    os.Getenv("CINEVOTE_ADMIN_PASSWORD"),
		RegistrationCode: os.Getenv("CINEVOTE_REGISTRATION_CODE"),
		PosterSource:     strings.ToLower(strings.TrimSpace(os.Getenv("CINEVOTE_POSTER_SOURCE"))),
		SharedPassword:   os.Getenv("CINEVOTE_PASSWORD"),
		Mattermost: MattermostSettings{
			URL:   strings.TrimRight(strings.TrimSpace(os.Getenv("MATTERMOST_URL")), "/"),
			Token: strings.TrimSpace(os.Getenv("MATTERMOST_TOKEN")),
		},
		OMDbAPIKey: strings.TrimSpace(os.Getenv("OMDB_API_KEY")),
		TMDBAPIKey: strings.TrimSpace(os.Getenv("TMDB_API_KEY")),
	}

	votes, err := envInt("CINEVOTE_MAX_VOTES", 5)
	if err != nil {
		return c, err
	}
	if votes < 1 {
		return c, fmt.Errorf("CINEVOTE_MAX_VOTES must be at least 1, got %d", votes)
	}
	c.MaxVotes = votes

	days, err := envInt("CINEVOTE_SESSION_DAYS", 30)
	if err != nil {
		return c, err
	}
	if days < 1 {
		return c, fmt.Errorf("CINEVOTE_SESSION_DAYS must be at least 1, got %d", days)
	}
	c.SessionTTL = time.Duration(days) * 24 * time.Hour

	c.SecureCookies = envBool("CINEVOTE_SECURE_COOKIES", false)
	c.Demo = envBool("CINEVOTE_DEMO", false)

	// Half a Mattermost configuration is worse than none: it looks connected
	// and silently is not, so say so at startup instead.
	if (c.Mattermost.URL == "") != (c.Mattermost.Token == "") {
		return c, errors.New("set both MATTERMOST_URL and MATTERMOST_TOKEN, or neither")
	}
	if c.Mattermost.Enabled() && c.SharedPassword == "" {
		return c, errors.New("CINEVOTE_PASSWORD must be set when Mattermost identifies people: " +
			"it is the one password everyone signs in with")
	}
	return c, nil
}

// UseMattermost reports whether people identify themselves with a chat account
// instead of a CineVote account.
func (c Config) UseMattermost() bool { return c.Mattermost.Enabled() }

// DefaultDBPath is where the database lives when CINEVOTE_DB says nothing.
const DefaultDBPath = "data/cinevote.db"

// DemoDBPath is the throwaway database demo mode always uses.
func DemoDBPath() string {
	return filepath.Join(os.TempDir(), "cinevote-demo.db")
}

// ApplyDemoDefaults relaxes everything a demo should not have to configure.
//
// The database is always the throwaway one, whatever CINEVOTE_DB says: a demo
// that reseeds on every start is the whole point, and it also means the demo
// can never touch a real database by accident. Returns the path it replaced, if
// any, so the caller can say so out loud.
func (c *Config) ApplyDemoDefaults(adminPassword string) (replacedDB string) {
	c.Demo = true
	// Only worth mentioning if someone actually chose a path; the default is
	// not something they asked for.
	if c.DBPath != DemoDBPath() && c.DBPath != DefaultDBPath {
		replacedDB = c.DBPath
	}
	c.DBPath = DemoDBPath()
	c.FreshDB = true

	if c.AdminPassword == "" {
		c.AdminPassword = adminPassword
	}
	// An invite code would defeat the purpose of a click-and-try demo.
	c.RegistrationCode = ""
	// The demo runs on its own seeded accounts and must never touch a real
	// chat server, the same rule the sibling sites keep.
	c.Mattermost = MattermostSettings{}
	return replacedDB
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("%s: %q is not a number", key, raw)
	}
	return n, nil
}

func envBool(key string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return b
}
