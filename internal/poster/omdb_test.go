package poster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
	return &Service{provider: client}
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
