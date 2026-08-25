package demo

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/o5ten/cinevote/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.UpsertAdmin(context.Background(), "admin", "hash"); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	return st
}

func TestSeedBuildsACompleteBoard(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	seeded, err := Seed(ctx, st, "admin")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !seeded {
		t.Fatal("first seed should report that it wrote data")
	}

	got, err := st.Movies(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(movies) {
		t.Fatalf("seeded %d movies, want %d", len(got), len(movies))
	}

	users, err := st.UserStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != len(Accounts("admin")) {
		t.Fatalf("seeded %d accounts, want %d", len(users), len(Accounts("admin")))
	}

	// Every film should carry the metadata that makes the demo look finished.
	var seen, unvoted int
	for _, m := range got {
		if m.PosterURL == "" || m.IMDbID == "" || m.Rating == "" || m.Overview == "" {
			t.Errorf("%s is missing demo metadata: %+v", m.Title, m)
		}
		if m.SuggestedBy == "" {
			t.Errorf("%s has no suggester", m.Title)
		}
		if m.Seen {
			seen++
			if m.Votes == 0 {
				t.Errorf("%s is marked seen but nobody voted for it", m.Title)
			}
		}
		if !m.Seen && m.Votes == 0 {
			unvoted++
		}
	}
	if seen == 0 {
		t.Error("the demo should include at least one watched film")
	}
	if unvoted == 0 {
		t.Error("the demo should include a film nobody has voted for yet")
	}
}

// The seed has to obey the same vote rules as a real user, including the
// budget, and it must leave at least one account with nothing left to spend.
func TestSeedRespectsVoteBudget(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := Seed(ctx, st, "admin"); err != nil {
		t.Fatal(err)
	}

	users, err := st.UserStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spent := 0
	for _, u := range users {
		if u.VotesUsed > st.MaxVotes {
			t.Errorf("%s uses %d votes, over the budget of %d", u.Username, u.VotesUsed, st.MaxVotes)
		}
		if u.VotesUsed == st.MaxVotes {
			spent++
		}
		if u.VotesUsed == 0 {
			t.Errorf("%s has not voted, the demo should show everyone participating", u.Username)
		}
	}
	if spent == 0 {
		t.Error("one demo account should have spent every vote, to show that state")
	}
}

func TestSeedProducesAClearTopThree(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := Seed(ctx, st, "admin"); err != nil {
		t.Fatal(err)
	}

	got, err := st.Movies(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	var top []store.Movie
	for _, m := range got {
		if !m.Seen && m.Rank > 0 && m.Rank <= 3 {
			top = append(top, m)
		}
	}
	if len(top) != 3 {
		t.Fatalf("got %d ranked films in the top three, want 3", len(top))
	}
	if top[0].Votes <= top[1].Votes {
		t.Errorf("the leader should be unambiguous: %s (%d) vs %s (%d)",
			top[0].Title, top[0].Votes, top[1].Title, top[1].Votes)
	}
	for i := 1; i < len(top); i++ {
		if top[i-1].Votes < top[i].Votes {
			t.Errorf("top three is not ordered by votes: %v", top)
		}
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	if _, err := Seed(ctx, st, "admin"); err != nil {
		t.Fatal(err)
	}
	before, err := st.Movies(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	seeded, err := Seed(ctx, st, "admin")
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if seeded {
		t.Error("second seed should report that it left the data alone")
	}
	after, err := st.Movies(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("movies went from %d to %d on re-seed", len(before), len(after))
	}
}

// A deployment may run with a tighter budget than the seed assumes; running
// out of votes must not break startup.
func TestSeedSurvivesASmallVoteBudget(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	st.MaxVotes = 1

	if _, err := Seed(ctx, st, "admin"); err != nil {
		t.Fatalf("seed with a budget of one: %v", err)
	}
	users, err := st.UserStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.VotesUsed > 1 {
			t.Errorf("%s uses %d votes with a budget of 1", u.Username, u.VotesUsed)
		}
	}
}

func TestSeedUsesTheConfiguredAdminName(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.UpsertAdmin(ctx, "filmchefen", "hash"); err != nil {
		t.Fatal(err)
	}

	if _, err := Seed(ctx, st, "filmchefen"); err != nil {
		t.Fatalf("seed with a renamed admin: %v", err)
	}
	for _, acc := range Accounts("filmchefen") {
		if acc.IsAdmin && acc.Username != "filmchefen" {
			t.Errorf("account list advertises %q as admin", acc.Username)
		}
	}

	// The admin's own votes must have landed on the renamed account.
	users, err := st.UserStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Username == "filmchefen" && u.VotesUsed == 0 {
			t.Error("the admin should have votes in the demo data")
		}
	}
}

func TestSeedFailsLoudlyWithoutAnAdmin(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := Seed(ctx, st, "admin"); err == nil {
		t.Error("seeding without an admin account should fail rather than half-populate")
	}
}
