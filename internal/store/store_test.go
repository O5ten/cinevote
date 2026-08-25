package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// timeIn returns a timestamp hours from now, for session expiry tests.
func timeIn(hours int) time.Time {
	return time.Now().Add(time.Duration(hours) * time.Hour)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustUser(t *testing.T, st *Store, name string) *User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), name, "hash-"+name, false)
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u
}

func mustMovie(t *testing.T, st *Store, title string, by int64) int64 {
	t.Helper()
	id, err := st.AddMovie(context.Background(), NewMovie{Title: title, SuggestedBy: by})
	if err != nil {
		t.Fatalf("add movie %s: %v", title, err)
	}
	return id
}

func TestUsersAreUnique(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	mustUser(t, st, "Ada")

	// Same name in different case is the same person.
	if _, err := st.CreateUser(ctx, "ada", "x", false); !errors.Is(err, ErrDuplicateUser) {
		t.Fatalf("want ErrDuplicateUser, got %v", err)
	}
}

func TestOnlyOneAdmin(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	first, err := st.CreateUser(ctx, "boss", "h", true)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.UpsertAdmin(ctx, "admin", "hash"); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	demoted, err := st.UserByID(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if demoted.IsAdmin {
		t.Error("previous admin should have been demoted: the app allows exactly one")
	}
	admin, err := st.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !admin.IsAdmin {
		t.Error("upserted account should be admin")
	}
}

func TestDuplicateMovieRejected(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, st, "Ada")

	if _, err := st.AddMovie(ctx, NewMovie{Title: "Alien", Year: "1979", SuggestedBy: u.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddMovie(ctx, NewMovie{Title: "alien", Year: "1979", SuggestedBy: u.ID}); !errors.Is(err, ErrDuplicateFilm) {
		t.Fatalf("want ErrDuplicateFilm, got %v", err)
	}
	// A remake in another year is a different film.
	if _, err := st.AddMovie(ctx, NewMovie{Title: "Alien", Year: "2026", SuggestedBy: u.ID}); err != nil {
		t.Fatalf("same title different year should be allowed: %v", err)
	}
}

func TestVoteBudget(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, st, "Ada")

	var ids []int64
	for _, title := range []string{"A", "B", "C", "D", "E", "F"} {
		ids = append(ids, mustMovie(t, st, title, u.ID))
	}

	for i := 0; i < DefaultMaxVotes; i++ {
		if err := st.Vote(ctx, u.ID, ids[i]); err != nil {
			t.Fatalf("vote %d: %v", i, err)
		}
	}
	if err := st.Vote(ctx, u.ID, ids[DefaultMaxVotes]); !errors.Is(err, ErrNoVotesLeft) {
		t.Fatalf("vote %d should exhaust the budget, got %v", DefaultMaxVotes+1, err)
	}

	used, err := st.VotesUsed(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != DefaultMaxVotes {
		t.Fatalf("votes used = %d, want %d", used, DefaultMaxVotes)
	}
}

func TestOneVotePerMovie(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, st, "Ada")
	id := mustMovie(t, st, "Solaris", u.ID)

	if err := st.Vote(ctx, u.ID, id); err != nil {
		t.Fatal(err)
	}
	if err := st.Vote(ctx, u.ID, id); !errors.Is(err, ErrAlreadyVoted) {
		t.Fatalf("want ErrAlreadyVoted, got %v", err)
	}

	if err := st.Unvote(ctx, u.ID, id); err != nil {
		t.Fatalf("unvote: %v", err)
	}
	if left, _ := st.VotesLeft(ctx, u.ID); left != DefaultMaxVotes {
		t.Fatalf("votes left after unvote = %d, want %d", left, DefaultMaxVotes)
	}
}

// The core requirement: marking a movie as seen hands the voters their votes
// back, while the record of who voted for it survives.
func TestSeenReturnsVotes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ada := mustUser(t, st, "Ada")
	bo := mustUser(t, st, "Bo")

	watched := mustMovie(t, st, "Stalker", ada.ID)
	other := mustMovie(t, st, "Arrival", ada.ID)

	for _, u := range []*User{ada, bo} {
		if err := st.Vote(ctx, u.ID, watched); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Vote(ctx, ada.ID, other); err != nil {
		t.Fatal(err)
	}
	if used, _ := st.VotesUsed(ctx, ada.ID); used != 2 {
		t.Fatalf("votes used before = %d, want 2", used)
	}

	if err := st.SetSeen(ctx, watched, true); err != nil {
		t.Fatalf("set seen: %v", err)
	}

	if used, _ := st.VotesUsed(ctx, ada.ID); used != 1 {
		t.Errorf("Ada should have one vote back, uses %d", used)
	}
	if used, _ := st.VotesUsed(ctx, bo.ID); used != 0 {
		t.Errorf("Bo should have all votes back, uses %d", used)
	}

	movies, err := st.Movies(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range movies {
		if m.ID != watched {
			continue
		}
		if !m.Seen {
			t.Error("movie should be marked seen")
		}
		if m.Votes != 2 || len(m.Voters) != 2 {
			t.Errorf("history lost: votes=%d voters=%v", m.Votes, m.Voters)
		}
	}

	// Voting for something already watched is not allowed.
	if err := st.Vote(ctx, bo.ID, watched); !errors.Is(err, ErrMovieSeen) {
		t.Fatalf("want ErrMovieSeen, got %v", err)
	}
}

// Re-opening a seen movie must not push a voter over the limit: they may have
// already spent the vote that was handed back.
func TestUnseenDropsOverBudgetVotes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	st.MaxVotes = 2
	u := mustUser(t, st, "Ada")

	watched := mustMovie(t, st, "Dune", u.ID)
	a := mustMovie(t, st, "Nope", u.ID)
	b := mustMovie(t, st, "Tenet", u.ID)

	if err := st.Vote(ctx, u.ID, watched); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSeen(ctx, watched, true); err != nil {
		t.Fatal(err)
	}
	// Vote budget was returned, so both of these fit.
	for _, id := range []int64{a, b} {
		if err := st.Vote(ctx, u.ID, id); err != nil {
			t.Fatalf("vote: %v", err)
		}
	}

	if err := st.SetSeen(ctx, watched, false); err != nil {
		t.Fatal(err)
	}
	used, err := st.VotesUsed(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != st.MaxVotes {
		t.Fatalf("votes used after reopening = %d, want %d (the returned vote was already spent)", used, st.MaxVotes)
	}
}

func TestMoviesRankedByVotes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ada := mustUser(t, st, "Ada")
	bo := mustUser(t, st, "Bo")
	cle := mustUser(t, st, "Cleo")

	winner := mustMovie(t, st, "Winner", ada.ID)
	second := mustMovie(t, st, "Second", ada.ID)
	mustMovie(t, st, "Nobody", ada.ID)
	watched := mustMovie(t, st, "Watched", ada.ID)

	for _, u := range []*User{ada, bo, cle} {
		if err := st.Vote(ctx, u.ID, winner); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []*User{ada, bo} {
		if err := st.Vote(ctx, u.ID, second); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Vote(ctx, cle.ID, watched); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSeen(ctx, watched, true); err != nil {
		t.Fatal(err)
	}

	movies, err := st.Movies(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(movies) != 4 {
		t.Fatalf("got %d movies, want 4", len(movies))
	}

	wantOrder := []string{"Winner", "Second", "Nobody", "Watched"}
	for i, want := range wantOrder {
		if movies[i].Title != want {
			t.Errorf("position %d = %q, want %q", i, movies[i].Title, want)
		}
	}
	// Ranks cover unseen movies only, so the seen one gets rank 0.
	for i, m := range movies[:3] {
		if m.Rank != i+1 {
			t.Errorf("%s rank = %d, want %d", m.Title, m.Rank, i+1)
		}
	}
	if movies[3].Rank != 0 {
		t.Errorf("seen movie should not be ranked, got %d", movies[3].Rank)
	}
	if !movies[0].VotedByMe {
		t.Error("VotedByMe should be set for the viewer's own vote")
	}
}

func TestSessions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, st, "Ada")

	expired := timeIn(-1)
	if err := st.CreateSession(ctx, u.ID, "stale", "csrf", expired); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Session(ctx, "stale"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session should not resolve, got %v", err)
	}

	if err := st.CreateSession(ctx, u.ID, "fresh", "csrf-token", timeIn(1)); err != nil {
		t.Fatal(err)
	}
	got, csrf, err := st.Session(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID || csrf != "csrf-token" {
		t.Fatalf("session mismatch: user=%d csrf=%q", got.ID, csrf)
	}

	n, err := st.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reaped %d sessions, want 1", n)
	}
	if _, _, err := st.Session(ctx, "fresh"); err != nil {
		t.Fatalf("live session should survive the reaper: %v", err)
	}
}

func TestDeleteUserFreesVotes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ada := mustUser(t, st, "Ada")
	bo := mustUser(t, st, "Bo")
	id := mustMovie(t, st, "Heat", ada.ID)

	for _, u := range []*User{ada, bo} {
		if err := st.Vote(ctx, u.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeleteUser(ctx, bo.ID); err != nil {
		t.Fatal(err)
	}
	m, err := st.MovieByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Votes != 1 {
		t.Fatalf("votes after deleting a user = %d, want 1", m.Votes)
	}
	if m.SuggestedBy != "Ada" {
		t.Fatalf("suggested by = %q, want Ada", m.SuggestedBy)
	}
}
