// Package demo fills a fresh database with a believable movie night: a handful
// of accounts, ten films with real posters and metadata, votes spread over
// them and a couple of films already watched.
//
// The point is a zero-configuration start: no API key, no admin password, no
// database setup. Run the binary with -demo and log in.
package demo

import (
	"context"
	"errors"
	"fmt"

	"github.com/mikaelo/cinevote/internal/auth"
	"github.com/mikaelo/cinevote/internal/store"
)

// Password is the shared password for every demo account, admin included. It
// is deliberately obvious: demo mode is not for real movie nights.
const Password = "demo1234"

// Account is one demo login, shown on the login page so nobody has to guess.
type Account struct {
	Username string
	IsAdmin  bool
	Note     string
}

// Account names used by the seed data below.
const (
	anna  = "Anna"
	bjorn = "Björn"
	cissi = "Cissi"
	david = "David"
	// adminKey stands in for the configured admin username while seeding.
	adminKey = "admin"
)

// Accounts lists the logins to show in the UI. adminUsername is whatever the
// deployment configured, so the page never advertises the wrong name.
func Accounts(adminUsername string) []Account {
	return []Account{
		{Username: adminUsername, IsAdmin: true, Note: "kan markera filmer som sedda"},
		{Username: anna, Note: "har lagt in flest förslag"},
		{Username: bjorn},
		{Username: cissi},
		{Username: david, Note: "har lagt ut alla sina röster"},
	}
}

// seedMovie is one film plus who suggested it and who backed it.
type seedMovie struct {
	Title    string
	Year     string
	IMDbID   string
	Rating   string
	Runtime  string
	Genres   string
	Overview string
	Poster   string

	SuggestedBy string   // account name
	Voters      []string // account names
	Seen        bool     // already watched: its votes have been handed back
}

// Seed populates an empty store. It is a no-op when any movie already exists,
// so restarting against a persistent database does not duplicate anything.
//
// Order matters: the films that have already been watched are voted for and
// marked seen first, which hands those votes back before the current round is
// voted on. That is the sequence a real movie night goes through, and it keeps
// the seed inside the vote budget at every step.
func Seed(ctx context.Context, st *store.Store, adminUsername string) (bool, error) {
	existing, err := st.Movies(ctx, 0)
	if err != nil {
		return false, fmt.Errorf("check for existing movies: %w", err)
	}
	if len(existing) > 0 {
		return false, nil
	}

	hash, err := auth.HashPassword(Password)
	if err != nil {
		return false, err
	}

	// The admin already exists (the binary bootstraps it before seeding), so
	// only the regular accounts are created here.
	ids := map[string]int64{}
	for _, name := range []string{anna, bjorn, cissi, david} {
		user, err := st.CreateUser(ctx, name, hash, false)
		switch {
		case err == nil:
			ids[name] = user.ID
		case errors.Is(err, store.ErrDuplicateUser):
			found, err := st.UserByUsername(ctx, name)
			if err != nil {
				return false, fmt.Errorf("look up demo user %s: %w", name, err)
			}
			ids[name] = found.ID
		default:
			return false, fmt.Errorf("create demo user %s: %w", name, err)
		}
	}
	admin, err := st.UserByUsername(ctx, adminUsername)
	if err != nil {
		return false, fmt.Errorf("look up admin %q: %w", adminUsername, err)
	}
	ids[adminKey] = admin.ID

	for _, watchedPass := range []bool{true, false} {
		for _, m := range movies {
			if m.Seen != watchedPass {
				continue
			}
			if err := seedMovieRow(ctx, st, ids, m); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func seedMovieRow(ctx context.Context, st *store.Store, ids map[string]int64, m seedMovie) error {
	id, err := st.AddMovie(ctx, store.NewMovie{
		Title:       m.Title,
		Year:        m.Year,
		PosterURL:   m.Poster,
		Overview:    m.Overview,
		IMDbID:      m.IMDbID,
		Rating:      m.Rating,
		Runtime:     m.Runtime,
		Genres:      m.Genres,
		SuggestedBy: ids[m.SuggestedBy],
	})
	if err != nil {
		return fmt.Errorf("add demo movie %s: %w", m.Title, err)
	}

	for _, voter := range m.Voters {
		userID, ok := ids[voter]
		if !ok {
			return fmt.Errorf("demo movie %s references unknown voter %q", m.Title, voter)
		}
		// A deployment may run with a smaller vote budget than the seed data
		// assumes; running out is expected there, not an error.
		if err := st.Vote(ctx, userID, id); err != nil && !errors.Is(err, store.ErrNoVotesLeft) {
			return fmt.Errorf("demo vote by %s on %s: %w", voter, m.Title, err)
		}
	}

	if m.Seen {
		if err := st.SetSeen(ctx, id, true); err != nil {
			return fmt.Errorf("mark demo movie %s as seen: %w", m.Title, err)
		}
	}
	return nil
}
