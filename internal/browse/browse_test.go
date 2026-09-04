package browse

import (
	"strings"
	"testing"
	"time"

	"github.com/o5ten/cinevote/internal/store"
)

// board is a small, deliberately varied set: two Villeneuve films, a shared
// actor, one film with no rating, one already watched.
func board() []store.Movie {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return []store.Movie{
		{
			ID: 1, Title: "Dune: Part One", Year: "2021", Rating: "8.0", Votes: 4,
			Genres: "Action, Adventure, Drama", Director: "Denis Villeneuve",
			Actors: "Timothée Chalamet, Rebecca Ferguson, Zendaya", CreatedAt: base,
		},
		{
			ID: 2, Title: "Blade Runner 2049", Year: "2017", Rating: "8.1", Votes: 3,
			Genres: "Action, Drama, Mystery", Director: "Denis Villeneuve",
			Actors: "Ryan Gosling, Harrison Ford, Ana de Armas", CreatedAt: base.Add(time.Hour),
		},
		{
			ID: 3, Title: "Arrival", Year: "2016", Rating: "7.9", Votes: 2,
			Genres: "Drama, Mystery, Sci-Fi", Director: "Denis Villeneuve",
			Actors: "Amy Adams, Jeremy Renner, Forest Whitaker", CreatedAt: base.Add(2 * time.Hour),
		},
		{
			ID: 4, Title: "The Grand Budapest Hotel", Year: "2014", Rating: "8.1", Votes: 1,
			Genres: "Comedy, Drama", Director: "Wes Anderson",
			Actors: "Ralph Fiennes, F. Murray Abraham, Mathieu Amalric", CreatedAt: base.Add(3 * time.Hour),
		},
		{
			ID: 5, Title: "Hemmagjord film", Year: "", Rating: "", Votes: 0,
			Genres: "", Director: "", Actors: "", CreatedAt: base.Add(4 * time.Hour),
		},
		{
			ID: 6, Title: "La La Land", Year: "2016", Rating: "8.0", Votes: 5,
			Genres: "Comedy, Drama, Music", Director: "Damien Chazelle",
			Actors: "Ryan Gosling, Emma Stone, Rosemarie DeWitt",
			Seen:   true, CreatedAt: base.Add(5 * time.Hour),
		},
	}
}

func titles(movies []store.Movie) []string {
	out := make([]string, 0, len(movies))
	for _, m := range movies {
		out = append(out, m.Title)
	}
	return out
}

func TestParseQueryDefaults(t *testing.T) {
	q := ParseQuery(map[string][]string{})
	if q.Sort != SortVotes || q.Show != ShowOpen {
		t.Errorf("unexpected defaults: %+v", q)
	}
	if q.Active() {
		t.Error("an empty query must not count as filtering")
	}
}

func TestParseQueryIgnoresNonsense(t *testing.T) {
	q := ParseQuery(map[string][]string{
		"sort":       {"by-vibes"},
		"show":       {"everything"},
		"min_rating": {"eleven"},
	})
	if q.Sort != SortVotes || q.Show != ShowOpen || q.MinRating != 0 {
		t.Errorf("nonsense should fall back to defaults, got %+v", q)
	}

	// A rating over 10 is not a rating.
	if got := ParseQuery(map[string][]string{"min_rating": {"11"}}); got.MinRating != 0 {
		t.Errorf("min_rating = %v, want 0", got.MinRating)
	}
	// Swedish decimal commas are common in this UI's language.
	if got := ParseQuery(map[string][]string{"min_rating": {"8,5"}}); got.MinRating != 8.5 {
		t.Errorf("min_rating = %v, want 8.5", got.MinRating)
	}
}

func TestHidesSeenByDefault(t *testing.T) {
	got := Apply(board(), ParseQuery(map[string][]string{}))
	for _, m := range got {
		if m.Seen {
			t.Errorf("%s is seen and should be hidden by default", m.Title)
		}
	}
	if len(got) != 5 {
		t.Fatalf("got %d films, want the 5 unseen ones: %v", len(got), titles(got))
	}
}

func TestShowSeenOnly(t *testing.T) {
	got := Apply(board(), ParseQuery(map[string][]string{"show": {"seen"}}))
	if len(got) != 1 || got[0].Title != "La La Land" {
		t.Fatalf("got %v, want just La La Land", titles(got))
	}
}

func TestFilterByGenre(t *testing.T) {
	got := Apply(board(), ParseQuery(map[string][]string{
		"genre": {"Mystery"}, "show": {"all"},
	}))
	if want := []string{"Blade Runner 2049", "Arrival"}; strings.Join(titles(got), ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", titles(got), want)
	}

	// Genre matching is on whole list items, not substrings: "Sci-Fi" must not
	// be found inside some other word, and case should not matter.
	if got := Apply(board(), ParseQuery(map[string][]string{"genre": {"sci-fi"}})); len(got) != 1 {
		t.Fatalf("case-insensitive genre match failed: %v", titles(got))
	}
}

func TestFilterByDirector(t *testing.T) {
	got := Apply(board(), ParseQuery(map[string][]string{
		"director": {"Denis Villeneuve"}, "show": {"all"},
	}))
	if len(got) != 3 {
		t.Fatalf("got %v, want the three Villeneuve films", titles(got))
	}
}

func TestFilterByMinRating(t *testing.T) {
	got := Apply(board(), ParseQuery(map[string][]string{"min_rating": {"8"}}))
	for _, m := range got {
		if r, ok := ratingOf(m); !ok || r < 8 {
			t.Errorf("%s (rating %q) should not pass an 8+ filter", m.Title, m.Rating)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %v, want the three 8+ films", titles(got))
	}
	// The unrated home movie must not sneak through a rating filter.
	for _, m := range got {
		if m.Title == "Hemmagjord film" {
			t.Error("a film with no rating should not pass a minimum-rating filter")
		}
	}
}

func TestFreeTextSearchesEveryField(t *testing.T) {
	cases := map[string]string{
		"villeneuve": "director",
		"gosling":    "cast",
		"budapest":   "title",
		"music":      "genre",
	}
	for term, what := range cases {
		got := Apply(board(), ParseQuery(map[string][]string{"q": {term}, "show": {"all"}}))
		if len(got) == 0 {
			t.Errorf("searching %q (%s) found nothing", term, what)
		}
	}

	// Every term has to match: this narrows to one film, not to everything
	// either word touches.
	got := Apply(board(), ParseQuery(map[string][]string{"q": {"villeneuve 2049"}, "show": {"all"}}))
	if len(got) != 1 || got[0].Title != "Blade Runner 2049" {
		t.Fatalf("got %v, want only Blade Runner 2049", titles(got))
	}
}

func TestSorting(t *testing.T) {
	cases := []struct {
		sort string
		want string // first title
	}{
		{SortVotes, "Dune: Part One"},
		{SortRating, "Blade Runner 2049"},
		{SortYear, "Dune: Part One"},
		{SortNewest, "Hemmagjord film"},
		{SortTitle, "Arrival"},
	}
	for _, tc := range cases {
		got := Apply(board(), ParseQuery(map[string][]string{"sort": {tc.sort}}))
		if len(got) == 0 || got[0].Title != tc.want {
			t.Errorf("sort=%s put %q first, want %q", tc.sort, titles(got)[0], tc.want)
		}
	}

	// Films with no rating sort last rather than as a zero.
	got := Apply(board(), ParseQuery(map[string][]string{"sort": {SortRating}}))
	if got[len(got)-1].Title != "Hemmagjord film" {
		t.Errorf("unrated film should sort last, order was %v", titles(got))
	}
}

func TestGenresAndDirectorsForTheDropdowns(t *testing.T) {
	genres := Genres(board())
	if len(genres) == 0 {
		t.Fatal("no genres collected")
	}
	for i := 1; i < len(genres); i++ {
		if strings.ToLower(genres[i-1]) > strings.ToLower(genres[i]) {
			t.Fatalf("genres are not sorted: %v", genres)
		}
	}
	for _, g := range genres {
		if g == "" || strings.Contains(g, ",") {
			t.Errorf("genre %q was not split cleanly", g)
		}
	}

	directors := Directors(board())
	if len(directors) != 3 {
		t.Fatalf("got %v, want three distinct directors", directors)
	}
	for _, d := range directors {
		if d == "" {
			t.Error("empty director should not reach the dropdown")
		}
	}
}

func TestSimilarPrefersTheSameDirector(t *testing.T) {
	all := board()
	dune := all[0]

	got := Similar(dune, all, 6)
	if len(got) == 0 {
		t.Fatal("no similar films found")
	}
	if got[0].Movie.Director != "Denis Villeneuve" {
		t.Errorf("top match is %q by %q, want another Villeneuve film",
			got[0].Movie.Title, got[0].Movie.Director)
	}
	if len(got[0].Reasons) == 0 {
		t.Error("a match must explain itself")
	}

	var joined string
	for _, r := range got[0].Reasons {
		joined += r + " | "
	}
	if !strings.Contains(joined, "Samma regissör") {
		t.Errorf("reasons should name the shared director, got %q", joined)
	}

	// The film itself is never its own suggestion.
	for _, m := range got {
		if m.Movie.ID == dune.ID {
			t.Error("a film matched itself")
		}
	}
	// Scores must not increase further down the list.
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Errorf("matches are not ordered by score: %d then %d", got[i-1].Score, got[i].Score)
		}
	}
}

func TestSimilarFindsSharedCastAcrossGenres(t *testing.T) {
	all := board()
	var bladeRunner store.Movie
	for _, m := range all {
		if m.Title == "Blade Runner 2049" {
			bladeRunner = m
		}
	}

	// La La Land shares only Ryan Gosling and the Drama genre, but that is
	// enough to be a suggestion.
	matches := Similar(bladeRunner, all, 6)
	var found *Match
	for i := range matches {
		if matches[i].Movie.Title == "La La Land" {
			found = &matches[i]
			break
		}
	}
	if found == nil {
		t.Fatal("La La Land shares an actor with Blade Runner 2049 and should be suggested")
	}
	var joined string
	for _, r := range found.Reasons {
		joined += r + " | "
	}
	if !strings.Contains(joined, "Gosling") {
		t.Errorf("the shared actor should be named, got %q", joined)
	}
}

func TestSimilarSkipsFilmsWithNothingInCommon(t *testing.T) {
	all := board()
	var homemade store.Movie
	for _, m := range all {
		if m.Title == "Hemmagjord film" {
			homemade = m
		}
	}

	if got := Similar(homemade, all, 6); len(got) != 0 {
		t.Fatalf("a film with no metadata should match nothing, got %v", got)
	}
}

func TestSimilarRespectsTheLimit(t *testing.T) {
	all := board()
	if got := Similar(all[0], all, 2); len(got) > 2 {
		t.Fatalf("got %d matches, want at most 2", len(got))
	}
}

// With equal support, the better-reviewed film should be listed first.
func TestEqualVotesAreBrokenByRating(t *testing.T) {
	movies := []store.Movie{
		{ID: 1, Title: "Meh", Votes: 2, Rating: "5.5", Genres: "Drama"},
		{ID: 2, Title: "Great", Votes: 2, Rating: "8.7", Genres: "Drama"},
		{ID: 3, Title: "Unrated", Votes: 2, Rating: "", Genres: "Drama"},
		{ID: 4, Title: "Good", Votes: 2, Rating: "7.1", Genres: "Drama"},
	}
	got := titles(Apply(movies, ParseQuery(map[string][]string{})))
	want := []string{"Great", "Good", "Meh", "Unrated"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Equal votes and equal rating: the newer film goes first. Same rule the store
// applies, so the filtered board and the podium agree.
func TestEqualVotesAndRatingAreBrokenByYear(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	movies := []store.Movie{
		{ID: 1, Title: "Undated", Votes: 2, Rating: "8.1", Year: "", CreatedAt: base},
		{ID: 2, Title: "Older", Votes: 2, Rating: "8.1", Year: "1999", CreatedAt: base.Add(time.Hour)},
		{ID: 3, Title: "Newest", Votes: 2, Rating: "8.1", Year: "2024", CreatedAt: base.Add(2 * time.Hour)},
		{ID: 4, Title: "Newer", Votes: 2, Rating: "8.1", Year: "2015", CreatedAt: base.Add(3 * time.Hour)},
		{ID: 5, Title: "Better", Votes: 2, Rating: "9.0", Year: "1974", CreatedAt: base.Add(4 * time.Hour)},
	}
	got := titles(Apply(movies, ParseQuery(map[string][]string{})))
	want := []string{"Better", "Newest", "Newer", "Older", "Undated"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Same for equally similar films: recommend the better-reviewed one first.
func TestEqualSimilarityIsBrokenByRating(t *testing.T) {
	target := store.Movie{ID: 1, Title: "Target", Year: "2020", Rating: "8.0", Genres: "Drama, Sci-Fi"}
	movies := []store.Movie{
		target,
		{ID: 2, Title: "Weaker", Year: "2020", Rating: "6.0", Genres: "Drama, Sci-Fi"},
		{ID: 3, Title: "Stronger", Year: "2020", Rating: "8.4", Genres: "Drama, Sci-Fi"},
	}
	got := Similar(target, movies, 6)
	if len(got) != 2 {
		t.Fatalf("got %d matches, want 2", len(got))
	}
	if got[0].Movie.Title != "Stronger" {
		t.Errorf("first match is %q, want the higher-rated Stronger", got[0].Movie.Title)
	}
}
