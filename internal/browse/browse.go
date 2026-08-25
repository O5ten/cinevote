// Package browse turns a board full of movies into the view someone asked for:
// filtered by rating, genre, director or free text, sorted the way they want,
// and — for one film at a time — the other films on the board that resemble it.
//
// It works on the slice the store already returned, because a movie night list
// is tens of films, not millions. That keeps the SQL in one place and makes
// every rule here trivially testable.
package browse

import (
	"sort"
	"strconv"
	"strings"

	"github.com/o5ten/cinevote/internal/store"
)

// Sort keys accepted in the query string.
const (
	SortVotes  = "votes" // most backers first — the default
	SortRating = "rating"
	SortYear   = "year"
	SortNewest = "new"
	SortTitle  = "title"
)

// Show controls whether films that have already been watched are included.
const (
	ShowOpen = "open" // still in the running — the default
	ShowAll  = "all"
	ShowSeen = "seen"
)

// Query is a request for a particular slice of the board.
type Query struct {
	Text      string  // free text over title, director, cast, genre, plot
	Genre     string  // exact genre, as listed by Genres
	Director  string  // exact director, as listed by Directors
	MinRating float64 // 0 means "no minimum"
	Sort      string
	Show      string
}

// ParseQuery reads a Query from URL values, falling back to the defaults for
// anything missing or unrecognised.
func ParseQuery(values map[string][]string) Query {
	get := func(key string) string {
		if v, ok := values[key]; ok && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}

	q := Query{
		Text:     get("q"),
		Genre:    get("genre"),
		Director: get("director"),
		Sort:     strings.ToLower(get("sort")),
		Show:     strings.ToLower(get("show")),
	}
	if rating, err := strconv.ParseFloat(strings.Replace(get("min_rating"), ",", ".", 1), 64); err == nil {
		if rating > 0 && rating <= 10 {
			q.MinRating = rating
		}
	}
	switch q.Sort {
	case SortVotes, SortRating, SortYear, SortNewest, SortTitle:
	default:
		q.Sort = SortVotes
	}
	switch q.Show {
	case ShowOpen, ShowAll, ShowSeen:
	default:
		q.Show = ShowOpen
	}
	return q
}

// Active reports whether the query asks for anything other than the plain
// board. The index page keeps its podium layout while this is false.
func (q Query) Active() bool {
	return q.Text != "" || q.Genre != "" || q.Director != "" ||
		q.MinRating > 0 || (q.Sort != "" && q.Sort != SortVotes) ||
		(q.Show != "" && q.Show != ShowOpen)
}

// MinRatingLabel renders the rating filter for display, without trailing zeros.
func (q Query) MinRatingLabel() string {
	if q.MinRating == 0 {
		return ""
	}
	return strconv.FormatFloat(q.MinRating, 'f', -1, 64)
}

// Apply filters and sorts a copy of the movies. The input order (already
// ranked by votes) decides ties, so equally-placed films keep a stable order.
func Apply(movies []store.Movie, q Query) []store.Movie {
	out := make([]store.Movie, 0, len(movies))
	for _, m := range movies {
		if !matches(m, q) {
			continue
		}
		out = append(out, m)
	}
	sortMovies(out, q.Sort)
	return out
}

func matches(m store.Movie, q Query) bool {
	switch q.Show {
	case ShowOpen:
		if m.Seen {
			return false
		}
	case ShowSeen:
		if !m.Seen {
			return false
		}
	}

	if q.Genre != "" && !hasItem(m.Genres, q.Genre) {
		return false
	}
	if q.Director != "" && !strings.EqualFold(strings.TrimSpace(m.Director), q.Director) {
		return false
	}
	if q.MinRating > 0 {
		rating, ok := ratingOf(m)
		if !ok || rating < q.MinRating {
			return false
		}
	}
	if q.Text != "" && !matchesText(m, q.Text) {
		return false
	}
	return true
}

// matchesText requires every whitespace-separated term to appear somewhere in
// the film's text, so "villeneuve 2049" narrows instead of widening.
func matchesText(m store.Movie, text string) bool {
	haystack := strings.ToLower(strings.Join([]string{
		m.Title, m.Year, m.Director, m.Actors, m.Genres, m.Overview, m.SuggestedBy,
	}, " "))
	for _, term := range strings.Fields(strings.ToLower(text)) {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func sortMovies(movies []store.Movie, key string) {
	switch key {
	case SortRating:
		sort.SliceStable(movies, func(i, j int) bool {
			a, aok := ratingOf(movies[i])
			b, bok := ratingOf(movies[j])
			if aok != bok {
				return aok // films without a rating sink to the bottom
			}
			return a > b
		})
	case SortYear:
		sort.SliceStable(movies, func(i, j int) bool {
			return yearOf(movies[i]) > yearOf(movies[j])
		})
	case SortNewest:
		sort.SliceStable(movies, func(i, j int) bool {
			return movies[i].CreatedAt.After(movies[j].CreatedAt)
		})
	case SortTitle:
		sort.SliceStable(movies, func(i, j int) bool {
			return strings.ToLower(movies[i].Title) < strings.ToLower(movies[j].Title)
		})
	default: // SortVotes
		sort.SliceStable(movies, func(i, j int) bool {
			if movies[i].Votes != movies[j].Votes {
				return movies[i].Votes > movies[j].Votes
			}
			// Equal support: put the better-reviewed film first.
			return betterRated(movies[i], movies[j])
		})
	}
}

// Genres lists every genre present on the board, for the filter dropdown.
func Genres(movies []store.Movie) []string {
	return uniqueItems(movies, func(m store.Movie) string { return m.Genres })
}

// Directors lists every director present on the board.
func Directors(movies []store.Movie) []string {
	return uniqueItems(movies, func(m store.Movie) string { return m.Director })
}

func uniqueItems(movies []store.Movie, field func(store.Movie) string) []string {
	seen := map[string]string{} // lowercase key -> first spelling seen
	for _, m := range movies {
		for _, item := range splitList(field(m)) {
			key := strings.ToLower(item)
			if _, ok := seen[key]; !ok {
				seen[key] = item
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, item := range seen {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

/* ------------------------------------------------------------- similarity --- */

// minSimilarScore is the bar a film has to clear to be called similar. One
// shared genre scores 2, which is why it is set above that: "both are Action
// films" is not a resemblance worth showing. A shared director (5), two shared
// genres (4), or a shared actor plus anything else all clear it.
const minSimilarScore = 4

// Match is one film that resembles another, with the reasons spelled out so the
// page can explain itself instead of showing an unexplained score.
type Match struct {
	Movie   store.Movie
	Score   int
	Reasons []string
}

// Similar ranks the other films on the board by how much they have in common
// with target: the same director counts most, then shared genres and cast,
// then era and rating. Films with nothing in common are left out.
func Similar(target store.Movie, movies []store.Movie, limit int) []Match {
	if limit <= 0 {
		limit = 6
	}

	var out []Match
	for _, candidate := range movies {
		if candidate.ID == target.ID {
			continue
		}
		if match := compare(target, candidate); match.Score >= minSimilarScore {
			out = append(out, match)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		// Equally similar: recommend the better-reviewed one first.
		if a, b := out[i].Movie, out[j].Movie; ratingDiffers(a, b) {
			return betterRated(a, b)
		}
		if out[i].Movie.Votes != out[j].Movie.Votes {
			return out[i].Movie.Votes > out[j].Movie.Votes
		}
		return strings.ToLower(out[i].Movie.Title) < strings.ToLower(out[j].Movie.Title)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func compare(target, candidate store.Movie) Match {
	m := Match{Movie: candidate}

	if d := strings.TrimSpace(target.Director); d != "" && strings.EqualFold(d, strings.TrimSpace(candidate.Director)) {
		m.Score += 5
		m.Reasons = append(m.Reasons, "Samma regissör: "+d)
	}

	if shared := overlap(target.Genres, candidate.Genres); len(shared) > 0 {
		m.Score += 2 * min(len(shared), 3)
		m.Reasons = append(m.Reasons, plural(len(shared), "Gemensam genre: ", "Gemensamma genrer: ")+strings.Join(shared, ", "))
	}

	if shared := overlap(target.Actors, candidate.Actors); len(shared) > 0 {
		m.Score += 2 * min(len(shared), 2)
		m.Reasons = append(m.Reasons, plural(len(shared), "Gemensam skådespelare: ", "Gemensamma skådespelare: ")+strings.Join(shared, ", "))
	}

	// Era and rating only refine an existing resemblance; on their own they
	// would match half the list.
	if m.Score > 0 {
		if a, b := yearOf(target), yearOf(candidate); a > 0 && b > 0 && a/10 == b/10 {
			m.Score++
			m.Reasons = append(m.Reasons, strconv.Itoa((a/10)*10)+"-talet, som originalet")
		}
		if a, aok := ratingOf(target); aok {
			if b, bok := ratingOf(candidate); bok && abs(a-b) <= 0.5 {
				m.Score++
				m.Reasons = append(m.Reasons, "Nästan samma betyg")
			}
		}
	}
	return m
}

/* ---------------------------------------------------------------- helpers --- */

// splitList reads OMDb's comma-separated fields ("Action, Drama").
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func hasItem(list, want string) bool {
	for _, item := range splitList(list) {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

// overlap returns the items both comma-separated lists contain, keeping the
// spelling from the first list.
func overlap(a, b string) []string {
	inB := map[string]bool{}
	for _, item := range splitList(b) {
		inB[strings.ToLower(item)] = true
	}
	var out []string
	for _, item := range splitList(a) {
		if inB[strings.ToLower(item)] {
			out = append(out, item)
		}
	}
	return out
}

// betterRated reports whether a should be listed before b on rating alone.
// A film with no rating always loses to one that has a rating.
func betterRated(a, b store.Movie) bool {
	ra, aok := ratingOf(a)
	rb, bok := ratingOf(b)
	if aok != bok {
		return aok
	}
	return ra > rb
}

func ratingDiffers(a, b store.Movie) bool {
	ra, aok := ratingOf(a)
	rb, bok := ratingOf(b)
	return aok != bok || ra != rb
}

func ratingOf(m store.Movie) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(m.Rating), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func yearOf(m store.Movie) int {
	v, err := strconv.Atoi(strings.TrimSpace(m.Year))
	if err != nil {
		return 0
	}
	return v
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
