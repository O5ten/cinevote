package poster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// omdbServer stands in for omdbapi.com, answering search and detail queries
// with the same field names and "N/A" sentinels the real API uses.
func omdbServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// The real API is case-insensitive on titles.
		search := strings.ToLower(q.Get("s"))
		title := strings.ToLower(q.Get("t"))
		if q.Get("apikey") == "" {
			t.Error("request reached OMDb without an api key")
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(search, "dune"):
			// Deliberately returns the weakest film first, so a rating-ranked
			// result set has to reorder it.
			w.Write([]byte(`{"Search":[
			  {"Title":"Dune","Year":"1984","imdbID":"tt0087182","Type":"movie","Poster":"https://example.test/d84.jpg"},
			  {"Title":"Dune: Part Two","Year":"2024","imdbID":"tt15239678","Type":"movie","Poster":"https://example.test/d2.jpg"},
			  {"Title":"Dune: Part One","Year":"2021","imdbID":"tt1160419","Type":"movie","Poster":"https://example.test/d1.jpg"},
			  {"Title":"Dune Drifter","Year":"2020","imdbID":"tt9999999","Type":"movie","Poster":"N/A"}],"Response":"True"}`))
		case q.Get("i") == "tt0087182":
			w.Write([]byte(`{"Title":"Dune","Year":"1984","Runtime":"137 min","Genre":"Adventure, Drama, Sci-Fi",
			  "Director":"David Lynch","Actors":"Kyle MacLachlan, Virginia Madsen, Francesca Annis",
			  "Plot":"A Duke's son leads desert warriors.","Poster":"https://example.test/d84.jpg",
			  "imdbRating":"6.3","imdbID":"tt0087182","Type":"movie","Response":"True"}`))
		case q.Get("i") == "tt15239678":
			w.Write([]byte(`{"Title":"Dune: Part Two","Year":"2024","Runtime":"166 min","Genre":"Action, Adventure, Drama",
			  "Director":"Denis Villeneuve","Actors":"Timothée Chalamet, Zendaya, Rebecca Ferguson",
			  "Plot":"Paul unites with the Fremen.","Poster":"https://example.test/d2.jpg",
			  "imdbRating":"8.5","imdbID":"tt15239678","Type":"movie","Response":"True"}`))
		case q.Get("i") == "tt1160419":
			w.Write([]byte(`{"Title":"Dune: Part One","Year":"2021","Runtime":"155 min","Genre":"Action, Adventure, Drama",
			  "Director":"Denis Villeneuve","Actors":"Timothée Chalamet, Rebecca Ferguson, Zendaya",
			  "Plot":"Paul Atreides arrives on Arrakis.","Poster":"https://example.test/d1.jpg",
			  "imdbRating":"8.0","imdbID":"tt1160419","Type":"movie","Response":"True"}`))
		case q.Get("i") == "tt9999999":
			// No rating at all, the way OMDb answers for obscure films.
			w.Write([]byte(`{"Title":"Dune Drifter","Year":"2020","Runtime":"N/A","Genre":"Sci-Fi",
			  "Director":"Marc Price","Actors":"Phoebe Sparrow","Plot":"N/A","Poster":"N/A",
			  "imdbRating":"N/A","imdbID":"tt9999999","Type":"movie","Response":"True"}`))
		case search == "blade runner":
			w.Write([]byte(`{"Search":[
			  {"Title":"Blade Runner","Year":"1982","imdbID":"tt0083658","Type":"movie",
			   "Poster":"https://example.test/br.jpg"},
			  {"Title":"Blade Runner 2049","Year":"2017","imdbID":"tt1856101","Type":"movie",
			   "Poster":"N/A"}],"totalResults":"2","Response":"True"}`))
		case search != "":
			w.Write([]byte(`{"Response":"False","Error":"Movie not found!"}`))
		case q.Get("i") == "tt0083658" || title == "blade runner":
			w.Write([]byte(`{"Title":"Blade Runner","Year":"1982","Runtime":"117 min",
			  "Genre":"Action, Drama, Sci-Fi","Plot":"A blade runner must pursue replicants.",
			  "Poster":"https://example.test/br.jpg","imdbRating":"8.1","imdbID":"tt0083658",
			  "Type":"movie","Response":"True"}`))
		default:
			w.Write([]byte(`{"Response":"False","Error":"Incorrect IMDb ID."}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testService(t *testing.T) *Service {
	t.Helper()
	client := NewOMDb("test-key", nil)
	client.base = omdbServer(t).URL + "/"
	// Wired the same way New does it, so request counting is exercised too.
	svc := &Service{provider: client, meter: &meter{}}
	client.meter = svc.meter
	return svc
}

func TestServiceDisabledWithoutKey(t *testing.T) {
	svc, err := New("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if svc.Enabled() {
		t.Error("service should be disabled with no API key")
	}
	if svc.Source() != SourceNone || svc.SourceLabel() != "" {
		t.Errorf("unexpected source %q", svc.Source())
	}
	// A disabled service is still safe to call.
	if results, err := svc.Search(context.Background(), "dune", 5); err != nil || results != nil {
		t.Errorf("disabled search should be a no-op, got %v %v", results, err)
	}
}

func TestSourceSelection(t *testing.T) {
	cases := []struct {
		name, source, omdb, tmdb string
		want                     Source
	}{
		{"auto prefers omdb", "", "k", "k", SourceIMDb},
		{"auto falls back to tmdb", "", "", "k", SourceTMDB},
		{"explicit tmdb", "tmdb", "k", "k", SourceTMDB},
		{"explicit imdb", "imdb", "k", "", SourceIMDb},
		{"imdb without key is disabled", "imdb", "", "k", SourceNone},
		{"none", "none", "k", "k", SourceNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := New(tc.source, tc.omdb, tc.tmdb)
			if err != nil {
				t.Fatal(err)
			}
			if got := svc.Source(); got != tc.want {
				t.Errorf("source = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := New("netflix", "k", "k"); err == nil {
		t.Error("unknown source should be rejected at startup")
	}
}

func TestOMDbSearch(t *testing.T) {
	svc := testService(t)
	results, err := svc.Search(context.Background(), "blade runner", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	first := results[0]
	if first.Title != "Blade Runner" || first.Year != "1982" || first.IMDbID != "tt0083658" {
		t.Errorf("unexpected first result: %+v", first)
	}
	if first.PosterURL != "https://example.test/br.jpg" {
		t.Errorf("poster = %q", first.PosterURL)
	}
	if first.IMDbURL() != "https://www.imdb.com/title/tt0083658/" {
		t.Errorf("imdb url = %q", first.IMDbURL())
	}
	// "N/A" is OMDb's way of saying "no poster"; it must not reach the page.
	if results[1].PosterURL != "" {
		t.Errorf("N/A poster should become empty, got %q", results[1].PosterURL)
	}
}

func TestOMDbSearchLimit(t *testing.T) {
	svc := testService(t)
	results, err := svc.Search(context.Background(), "blade runner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("limit ignored: got %d results", len(results))
	}
}

func TestOMDbNoHitsIsNotAnError(t *testing.T) {
	svc := testService(t)
	results, err := svc.Search(context.Background(), "nonexistent film", 8)
	if err != nil {
		t.Fatalf("a miss should not be an error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want none", len(results))
	}
}

func TestOMDbDetailAndBest(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	detail, err := svc.Detail(ctx, "tt0083658")
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil {
		t.Fatal("expected a detail result")
	}
	if detail.Rating != "8.1" || detail.Runtime != "117 min" || detail.Genres != "Action, Drama, Sci-Fi" {
		t.Errorf("detail fields missing: %+v", detail)
	}
	if detail.Overview == "" {
		t.Error("plot should be filled in")
	}

	// Best searches, prefers the exact title, then enriches it via Detail.
	best, err := svc.Best(ctx, "Blade Runner")
	if err != nil {
		t.Fatal(err)
	}
	if best == nil || best.IMDbID != "tt0083658" {
		t.Fatalf("unexpected best match: %+v", best)
	}
	if best.Rating == "" {
		t.Error("Best should enrich the winner with detail data")
	}
}

func TestFirstYear(t *testing.T) {
	cases := map[string]string{
		"1982":       "1982",
		"1999–2004":  "1999",
		"2010-":      "2010",
		"2017-05-05": "2017",
		"N/A":        "",
		"":           "",
	}
	for in, want := range cases {
		if got := firstYear(in); got != want {
			t.Errorf("firstYear(%q) = %q, want %q", in, got, want)
		}
	}
}

// A search result set should lead with the best-reviewed film, and the thin
// search response has to be filled in for that to be possible at all.
func TestSearchPrefersHigherRatedFilms(t *testing.T) {
	svc := testService(t)
	// A query that is not any film's exact title, so ranking is purely by rating.
	results, err := svc.Search(context.Background(), "dune saga", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}

	if results[0].Title != "Dune: Part Two" {
		t.Errorf("first result is %q, want the 8.5-rated Dune: Part Two", results[0].Title)
	}
	if results[len(results)-1].Title != "Dune Drifter" {
		t.Errorf("last result is %q, want the unrated Dune Drifter", results[len(results)-1].Title)
	}

	var ratings []float64
	for _, r := range results {
		if v, ok := r.rating(); ok {
			ratings = append(ratings, v)
		}
	}
	for i := 1; i < len(ratings); i++ {
		if ratings[i-1] < ratings[i] {
			t.Errorf("ratings are not descending: %v", ratings)
			break
		}
	}

	// Enrichment must also fill the fields the search response omits, which is
	// what the suggestion list displays.
	for _, r := range results[:3] {
		if r.Rating == "" || r.Genres == "" || r.Director == "" {
			t.Errorf("%s was not enriched: %+v", r.Title, r)
		}
	}
	// ...without losing what the search hit already had.
	if results[0].PosterURL == "" {
		t.Error("poster from the search hit was lost during enrichment")
	}
}

// An exact title match outranks a better-reviewed sequel: someone searching
// "Dune" means Dune.
func TestExactTitleMatchWinsOverRating(t *testing.T) {
	svc := testService(t)
	results, err := svc.Search(context.Background(), "Dune", 8)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Title != "Dune" {
		t.Errorf("first result is %q, want the exact match Dune", results[0].Title)
	}
	if results[1].Title != "Dune: Part Two" {
		t.Errorf("second result is %q, want the highest rated of the rest", results[1].Title)
	}
}

// Detail lookups are cached, so typing in the search box does not burn through
// a free API quota.
func TestDetailLookupsAreCached(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Title":"Dune: Part One","Year":"2021","imdbRating":"8.0","Genre":"Action",
		  "Director":"Denis Villeneuve","imdbID":"tt1160419","Type":"movie","Response":"True"}`))
	}))
	t.Cleanup(srv.Close)

	client := NewOMDb("test-key", nil)
	client.base = srv.URL + "/"
	svc := &Service{provider: client}

	for i := 0; i < 3; i++ {
		if _, err := svc.Detail(context.Background(), "tt1160419"); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("made %d API calls for the same film, want 1", got)
	}
}

// The API reports nothing about remaining quota, so the counter has to track
// requests we actually send — and cache hits are not requests.
func TestUsageCountsRequests(t *testing.T) {
	svc := testService(t)

	if got := svc.Usage(); got.Used != 0 || got.Limit != DailyRequestLimit || got.Remaining != DailyRequestLimit {
		t.Fatalf("fresh usage = %+v", got)
	}

	if _, err := svc.Detail(context.Background(), "tt0083658"); err != nil {
		t.Fatal(err)
	}
	after := svc.Usage()
	if after.Used != 1 || after.Remaining != DailyRequestLimit-1 {
		t.Fatalf("after one lookup: %+v", after)
	}

	// Same film again: served from cache, so the count must not move.
	if _, err := svc.Detail(context.Background(), "tt0083658"); err != nil {
		t.Fatal(err)
	}
	if cached := svc.Usage(); cached.Used != 1 {
		t.Errorf("a cache hit was counted as an API request: %+v", cached)
	}

	// A search is one request plus one enrichment per unrated hit.
	before := svc.Usage().Used
	results, err := svc.Search(context.Background(), "dune saga", 8)
	if err != nil {
		t.Fatal(err)
	}
	spent := svc.Usage().Used - before
	if spent != 1+len(results) {
		t.Errorf("search spent %d requests for %d results, want %d", spent, len(results), 1+len(results))
	}
}

func TestUsageFlagsRunningLow(t *testing.T) {
	m := &meter{}
	m.used = DailyRequestLimit - 50

	got := m.snapshot("IMDb", DailyRequestLimit)
	if !got.Low || got.Exhausted {
		t.Errorf("50 left should read as low but not exhausted: %+v", got)
	}
	if got.Remaining != 50 || got.Percent != 95 {
		t.Errorf("unexpected numbers: %+v", got)
	}
}

// When OMDb says the allowance is gone, that has to be its own error so the UI
// can explain it instead of blaming the network.
func TestQuotaExceededIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Response":"False","Error":"Request limit reached!"}`))
	}))
	t.Cleanup(srv.Close)

	client := NewOMDb("test-key", nil)
	client.base = srv.URL + "/"
	svc := &Service{provider: client, meter: &meter{}}
	client.meter = svc.meter

	_, err := svc.Search(context.Background(), "anything", 8)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	if got := svc.Usage(); !got.Exhausted || got.Remaining != 0 {
		t.Errorf("usage should read as exhausted: %+v", got)
	}
}

// Repeating a search must not spend the allowance again: it is the most
// expensive single action, at one request per result plus one for the search.
func TestRepeatedSearchIsFree(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	first, err := svc.Search(ctx, "blade runner", 8)
	if err != nil {
		t.Fatal(err)
	}
	spent := svc.Usage().Used
	if spent == 0 {
		t.Fatal("the first search should cost something")
	}

	second, err := svc.Search(ctx, "Blade Runner", 8) // different case, same query
	if err != nil {
		t.Fatal(err)
	}
	if svc.Usage().Used != spent {
		t.Errorf("a repeated search spent %d more requests", svc.Usage().Used-spent)
	}
	if len(second) != len(first) || second[0].Title != first[0].Title {
		t.Errorf("cached results differ: %v vs %v", first, second)
	}

	// The caller may sort what it gets without corrupting the cache.
	second[0] = Result{Title: "mutated"}
	third, err := svc.Search(ctx, "blade runner", 8)
	if err != nil {
		t.Fatal(err)
	}
	if third[0].Title == "mutated" {
		t.Error("the cache handed out a mutable reference")
	}
}
