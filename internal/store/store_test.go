package store

import (
	"context"
	"database/sql"
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
	if err := st.CreateSession(ctx, u.ID, "stale", "csrf", "", expired); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Session(ctx, "stale"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session should not resolve, got %v", err)
	}

	if err := st.CreateSession(ctx, u.ID, "fresh", "csrf-token", "", timeIn(1)); err != nil {
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

// A database created by an earlier version is missing the columns added since.
// CREATE TABLE IF NOT EXISTS leaves it alone, so opening it has to patch the
// schema rather than fail on the first query.
func TestOpenMigratesAnOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Stand up the movies table as an early build had it: no imdb_id, rating,
	// runtime, genres, director or actors.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
CREATE TABLE movies (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	title        TEXT    NOT NULL,
	title_ci     TEXT    NOT NULL,
	year         TEXT    NOT NULL DEFAULT '',
	poster_url   TEXT    NOT NULL DEFAULT '',
	overview     TEXT    NOT NULL DEFAULT '',
	tmdb_id      INTEGER,
	suggested_by INTEGER,
	seen         INTEGER NOT NULL DEFAULT 0,
	seen_at      INTEGER,
	created_at   INTEGER NOT NULL
);
INSERT INTO movies (title, title_ci, year, created_at) VALUES ('Alien', 'alien', '1979', 0);`); err != nil {
		t.Fatalf("build the old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open a database from an older version: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	movies, err := st.Movies(ctx, 0)
	if err != nil {
		t.Fatalf("query after migration: %v", err)
	}
	if len(movies) != 1 || movies[0].Title != "Alien" {
		t.Fatalf("existing row lost: %+v", movies)
	}
	if movies[0].Director != "" || movies[0].Rating != "" {
		t.Errorf("new columns should default to empty, got %+v", movies[0])
	}

	// And the new columns are writable.
	u := mustUser(t, st, "Ada")
	if _, err := st.AddMovie(ctx, NewMovie{
		Title: "Arrival", Rating: "7.9", Director: "Denis Villeneuve",
		Actors: "Amy Adams", Genres: "Drama", SuggestedBy: u.ID,
	}); err != nil {
		t.Fatalf("insert using the added columns: %v", err)
	}

	// Opening again must be a no-op, not a duplicate-column error.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	again.Close()
}

// A Mattermost identity becomes one CineVote account, reused on every sign-in
// and kept in step with the chat's display name.
func TestUpsertMattermostUser(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first, err := st.UpsertMattermostUser(ctx, "anna.andersson", "u1", "Anna Andersson")
	if err != nil {
		t.Fatal(err)
	}
	if !first.FromMattermost() || first.Name() != "Anna Andersson" {
		t.Fatalf("account = %+v", first)
	}
	if first.PasswordHash != "" {
		t.Error("a chat account should not carry a password")
	}

	// Signing in again is the same account, with the name refreshed.
	again, err := st.UpsertMattermostUser(ctx, "Anna.Andersson", "u1", "Anna Ö. Andersson")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("second sign-in made a new account: %d then %d", first.ID, again.ID)
	}
	if again.Name() != "Anna Ö. Andersson" {
		t.Errorf("display name = %q, want the one from the chat", again.Name())
	}

	users, err := st.UserStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d accounts, want 1", len(users))
	}

	// Password accounts are untouched by any of this.
	if _, err := st.CreateUser(ctx, "Bo", "hash", false); err != nil {
		t.Fatal(err)
	}
	bo, err := st.UserByUsername(ctx, "Bo")
	if err != nil {
		t.Fatal(err)
	}
	if bo.FromMattermost() || bo.Name() != "Bo" {
		t.Errorf("password account = %+v", bo)
	}
}

// The pages show the chat's name for a chat-identified member.
func TestDisplayNamesReachTheBoard(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	anna, err := st.UpsertMattermostUser(ctx, "anna.andersson", "u1", "Anna Andersson")
	if err != nil {
		t.Fatal(err)
	}
	id := mustMovie(t, st, "Stalker", anna.ID)
	if err := st.Vote(ctx, anna.ID, id); err != nil {
		t.Fatal(err)
	}

	movies, err := st.Movies(ctx, anna.ID)
	if err != nil {
		t.Fatal(err)
	}
	if movies[0].SuggestedBy != "Anna Andersson" {
		t.Errorf("suggested by %q, want the display name", movies[0].SuggestedBy)
	}
	if len(movies[0].Voters) != 1 || movies[0].Voters[0] != "Anna Andersson" {
		t.Errorf("voters = %v, want the display name", movies[0].Voters)
	}
}

// The session carries the role in Mattermost mode, where accounts have none.
func TestSessionRoleGrantsAdmin(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := mustUser(t, st, "Ada")

	if err := st.CreateSession(ctx, u.ID, "plain", "csrf", "", timeIn(1)); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.Session(ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if got.IsAdmin {
		t.Error("a session with no role should not be admin")
	}

	if err := st.CreateSession(ctx, u.ID, "boss", "csrf", RoleAdmin, timeIn(1)); err != nil {
		t.Fatal(err)
	}
	got, _, err = st.Session(ctx, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsAdmin {
		t.Error("an admin session should grant admin rights")
	}
	// The account itself is unchanged: the role lives on the session.
	stored, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IsAdmin {
		t.Error("the account should not have been promoted")
	}
}

// Films on equal votes are separated by the film itself: highest rating first,
// then the most recently released. This is what decides the premiere.
func TestEqualVotesArePremieredByRatingThenYear(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ada := mustUser(t, st, "Ada")

	add := func(title, year, rating string) int64 {
		t.Helper()
		id, err := st.AddMovie(ctx, NewMovie{
			Title: title, Year: year, Rating: rating, SuggestedBy: ada.ID,
		})
		if err != nil {
			t.Fatalf("add %s: %v", title, err)
		}
		return id
	}

	// Deliberately added worst-first, so the order cannot come from insertion.
	unrated := add("Unrated", "2024", "")
	older := add("Older", "1999", "8.1")
	newer := add("Newer", "2021", "8.1")
	best := add("Best", "1974", "9.2")

	// One vote each: nothing but rating and year can separate them.
	for _, id := range []int64{unrated, older, newer, best} {
		if err := st.Vote(ctx, ada.ID, id); err != nil {
			t.Fatalf("vote: %v", err)
		}
	}

	movies, err := st.Movies(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Best", "Newer", "Older", "Unrated"}
	for i, title := range want {
		if movies[i].Title != title {
			t.Errorf("position %d = %q, want %q (full order: %v)", i, movies[i].Title, title, titlesOf(movies))
		}
		if movies[i].Rank != i+1 {
			t.Errorf("%s has rank %d, want %d", movies[i].Title, movies[i].Rank, i+1)
		}
	}
}

// More votes still beat a better film: the rating only breaks a tie.
func TestVotesOutrankRating(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ada := mustUser(t, st, "Ada")
	bo := mustUser(t, st, "Bo")

	popular, err := st.AddMovie(ctx, NewMovie{Title: "Popular", Year: "2001", Rating: "6.0", SuggestedBy: ada.ID})
	if err != nil {
		t.Fatal(err)
	}
	acclaimed, err := st.AddMovie(ctx, NewMovie{Title: "Acclaimed", Year: "2001", Rating: "9.5", SuggestedBy: ada.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*User{ada, bo} {
		if err := st.Vote(ctx, u.ID, popular); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Vote(ctx, ada.ID, acclaimed); err != nil {
		t.Fatal(err)
	}

	movies, err := st.Movies(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if movies[0].Title != "Popular" {
		t.Fatalf("order = %v, want the film with more votes first", titlesOf(movies))
	}
}

// Equal on every count: the earliest suggestion stays ahead, so the order does
// not wobble between requests.
func TestIdenticalFilmsKeepAStableOrder(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ada := mustUser(t, st, "Ada")

	first, err := st.AddMovie(ctx, NewMovie{Title: "First", Year: "2020", Rating: "7.5", SuggestedBy: ada.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.AddMovie(ctx, NewMovie{Title: "Second", Year: "2020", Rating: "7.5", SuggestedBy: ada.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first, second} {
		if err := st.Vote(ctx, ada.ID, id); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 3; i++ {
		movies, err := st.Movies(ctx, ada.ID)
		if err != nil {
			t.Fatal(err)
		}
		if movies[0].Title != "First" {
			t.Fatalf("read %d gave %v, want the earlier suggestion first", i, titlesOf(movies))
		}
	}
}

func titlesOf(movies []Movie) []string {
	out := make([]string, 0, len(movies))
	for _, m := range movies {
		out = append(out, m.Title)
	}
	return out
}
