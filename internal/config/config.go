// Package config reads the runtime configuration from the environment so the
// same binary works unchanged in a container.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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

	// Demo mode: seed a populated database and show the logins in the UI.
	// Nothing needs to be configured for it, so it is also the mode that must
	// never be reachable by accident — only an explicit -demo flag or
	// CINEVOTE_DEMO=true turns it on.
	Demo bool
	// DBPathExplicit records whether CINEVOTE_DB was set, so demo mode knows
	// it may pick a throwaway location instead.
	DBPathExplicit bool
	// FreshDB asks the caller to delete the database before opening it, so a
	// demo always starts from the same state.
	FreshDB bool
}

func Load() (Config, error) {
	c := Config{
		Addr:             env("CINEVOTE_ADDR", ":8080"),
		DBPathExplicit:   strings.TrimSpace(os.Getenv("CINEVOTE_DB")) != "",
		DBPath:           env("CINEVOTE_DB", "data/cinevote.db"),
		SiteName:         env("CINEVOTE_SITE_NAME", "CineVote"),
		AdminUsername:    env("CINEVOTE_ADMIN_USERNAME", "admin"),
		AdminPassword:    os.Getenv("CINEVOTE_ADMIN_PASSWORD"),
		RegistrationCode: os.Getenv("CINEVOTE_REGISTRATION_CODE"),
		PosterSource:     strings.ToLower(strings.TrimSpace(os.Getenv("CINEVOTE_POSTER_SOURCE"))),
		OMDbAPIKey:       strings.TrimSpace(os.Getenv("OMDB_API_KEY")),
		TMDBAPIKey:       strings.TrimSpace(os.Getenv("TMDB_API_KEY")),
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
	return c, nil
}

// DemoDBPath is where a demo without CINEVOTE_DB keeps its throwaway database.
func DemoDBPath() string {
	return filepath.Join(os.TempDir(), "cinevote-demo.db")
}

// ApplyDemoDefaults relaxes everything a demo should not have to configure:
// a throwaway database that is recreated on every start, and a known admin
// password. Explicit settings always win.
func (c *Config) ApplyDemoDefaults(adminPassword string) {
	c.Demo = true
	if !c.DBPathExplicit {
		c.DBPath = DemoDBPath()
		c.FreshDB = true
	}
	if c.AdminPassword == "" {
		c.AdminPassword = adminPassword
	}
	// An invite code would defeat the purpose of a click-and-try demo.
	c.RegistrationCode = ""
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
