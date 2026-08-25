// Package poster looks up movie metadata and poster art from an external movie
// database, so a suggestion only needs a title typed into the form.
//
// Two backends are supported and both are optional:
//
//	imdb — OMDb (omdbapi.com), the free IMDb data API. Default when a key is set.
//	tmdb — The Movie Database, useful as a fallback or for non-English titles.
//
// With no API key configured the service reports Enabled() == false and the UI
// falls back to a manually pasted poster URL.
package poster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Source names a backend.
type Source string

const (
	SourceIMDb Source = "imdb"
	SourceTMDB Source = "tmdb"
	SourceNone Source = "none"
)

// Result is one candidate movie: what the search box shows and what we store.
type Result struct {
	Source    Source `json:"source"`
	IMDbID    string `json:"imdb_id"` // e.g. "tt0110912"
	TMDBID    int64  `json:"tmdb_id"` // 0 when unknown
	Title     string `json:"title"`
	Year      string `json:"year"`
	PosterURL string `json:"poster_url"`
	Overview  string `json:"overview"`
	Rating    string `json:"rating"`  // IMDb rating as text, e.g. "8.9"
	Runtime   string `json:"runtime"` // e.g. "154 min"
	Genres    string `json:"genres"`  // comma separated
	Director  string `json:"director"`
	Actors    string `json:"actors"` // comma separated, a few headline names
	// RatingVotes is how many people rated it, which is what makes the rating
	// trustworthy. Not to be confused with votes cast in CineVote itself.
	RatingVotes int64 `json:"rating_votes"`
}

// IMDbURL is the public page for a result, or "" when we have no IMDb id.
func (r Result) IMDbURL() string {
	if r.IMDbID == "" {
		return ""
	}
	return "https://www.imdb.com/title/" + r.IMDbID + "/"
}

// Provider is one metadata backend.
type Provider interface {
	Source() Source
	// Search returns candidates for a free-text title query.
	Search(ctx context.Context, query string, limit int) ([]Result, error)
	// Detail returns the full record for a provider-specific id (an IMDb
	// "tt…" id, or a TMDB numeric id as a string).
	Detail(ctx context.Context, id string) (*Result, error)
}

// ErrUnsupportedSource is returned when configuration names a backend we do
// not have.
var ErrUnsupportedSource = errors.New("unknown poster source")

// Service is the handle the rest of the app uses. A zero Service (or a nil
// one) is valid and simply disabled.
//
// The primary provider answers searches and lookups. TMDB is kept separately
// whenever a key for it exists, because it is the only backend that can answer
// "films like this one" — so an OMDb-first setup with a TMDB key gets IMDb
// metadata and TMDB recommendations at the same time.
type Service struct {
	provider Provider
	tmdb     *TMDB
	cache    detailCache
	searches searchCache
	meter    *meter
}

// enrichConcurrency caps how many detail lookups one search fires at once —
// enough to stay quick, few enough to be polite to a free API tier.
const enrichConcurrency = 4

// detailCache remembers detail lookups for the lifetime of the process. People
// searching for the same film repeatedly is the common case, and the free OMDb
// tier has a daily budget worth protecting.
type detailCache struct {
	mu      sync.Mutex
	entries map[string]Result
}

const detailCacheMax = 512

// searchCache remembers whole result sets. A search costs one request plus one
// per result to enrich, so repeating one is the most expensive thing a user can
// do by accident.
type searchCache struct {
	mu      sync.Mutex
	entries map[string][]Result
}

const searchCacheMax = 128

func (c *searchCache) get(key string) ([]Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	results, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	// Hand back a copy: callers sort and mutate what they get.
	out := make([]Result, len(results))
	copy(out, results)
	return out, true
}

func (c *searchCache) put(key string, results []Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string][]Result, 32)
	}
	if len(c.entries) >= searchCacheMax {
		c.entries = make(map[string][]Result, 32)
	}
	stored := make([]Result, len(results))
	copy(stored, results)
	c.entries[key] = stored
}

func (c *detailCache) get(id string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, ok := c.entries[id]
	return res, ok
}

func (c *detailCache) put(id string, res Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]Result, 64)
	}
	// Nothing clever: once it is full, start over rather than grow forever.
	if len(c.entries) >= detailCacheMax {
		c.entries = make(map[string]Result, 64)
	}
	c.entries[id] = res
}

// New builds the service. source may be "imdb", "tmdb" or "" (auto: IMDb when
// an OMDb key is present, otherwise TMDB). A missing key for the chosen
// backend disables lookups rather than failing startup — the app is perfectly
// usable with hand-pasted poster links.
func New(source, omdbKey, tmdbKey string) (*Service, error) {
	omdbKey, tmdbKey = strings.TrimSpace(omdbKey), strings.TrimSpace(tmdbKey)

	svc := &Service{meter: &meter{}}
	if tmdbKey != "" {
		svc.tmdb = NewTMDB(tmdbKey, nil)
		svc.tmdb.meter = svc.meter
	}

	switch Source(strings.ToLower(strings.TrimSpace(source))) {
	case SourceIMDb:
		if omdbKey != "" {
			client := NewOMDb(omdbKey, nil)
			client.meter = svc.meter
			svc.provider = client
		}
	case SourceTMDB:
		if svc.tmdb != nil {
			svc.provider = svc.tmdb
		}
	case SourceNone:
		svc.tmdb = nil
	case "":
		switch {
		case omdbKey != "":
			client := NewOMDb(omdbKey, nil)
			client.meter = svc.meter
			svc.provider = client
		case svc.tmdb != nil:
			svc.provider = svc.tmdb
		}
	default:
		return nil, fmt.Errorf("%w: %q (use \"imdb\" or \"tmdb\")", ErrUnsupportedSource, source)
	}
	return svc, nil
}

func (s *Service) Enabled() bool { return s != nil && s.provider != nil }

// Source reports which backend is active, for display in the UI.
func (s *Service) Source() Source {
	if !s.Enabled() {
		return SourceNone
	}
	return s.provider.Source()
}

// SourceLabel is the human name shown next to the search box.
func (s *Service) SourceLabel() string {
	switch s.Source() {
	case SourceIMDb:
		return "IMDb"
	case SourceTMDB:
		return "TMDB"
	}
	return ""
}

func (s *Service) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if !s.Enabled() || query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}

	// Typing towards a title fires several searches, and people search for the
	// same film repeatedly. Both are free after the first time.
	key := strings.ToLower(query) + "\x00" + strconv.Itoa(limit)
	if cached, ok := s.searches.get(key); ok {
		return cached, nil
	}

	results, err := s.provider.Search(ctx, query, limit)
	if err != nil || len(results) == 0 {
		return results, err
	}
	// A search response is thin — OMDb returns no rating at all — so fill the
	// gaps before ranking, otherwise "best rated first" has nothing to sort on.
	s.enrich(ctx, results)
	rankByRating(results, query)
	s.searches.put(key, results)
	return results, nil
}

// enrich fetches the details a search response leaves out for the entries that
// are missing a rating. Failures are ignored: a thin result is better than no
// result, and the cache keeps repeat typing off the API.
func (s *Service) enrich(ctx context.Context, results []Result) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, enrichConcurrency)

	for i := range results {
		if results[i].Rating != "" {
			continue
		}
		id := results[i].detailID()
		if id == "" {
			continue
		}
		if cached, ok := s.cache.get(id); ok {
			results[i] = merge(results[i], cached)
			continue
		}

		wg.Add(1)
		go func(idx int, id string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			full, err := s.provider.Detail(ctx, id)
			if err != nil || full == nil {
				return
			}
			s.cache.put(id, *full)
			results[idx] = merge(results[idx], *full)
		}(i, id)
	}
	wg.Wait()
}

// merge layers a detail response over a search hit, keeping whatever the search
// gave us for fields the detail left empty.
func merge(base, detail Result) Result {
	out := detail
	if out.Title == "" {
		out.Title = base.Title
	}
	if out.Year == "" {
		out.Year = base.Year
	}
	if out.PosterURL == "" {
		out.PosterURL = base.PosterURL
	}
	if out.IMDbID == "" {
		out.IMDbID = base.IMDbID
	}
	if out.TMDBID == 0 {
		out.TMDBID = base.TMDBID
	}
	if out.RatingVotes == 0 {
		out.RatingVotes = base.RatingVotes
	}
	return out
}

// Rating credibility tiers. A high score from a few hundred people says much
// less than a slightly lower one from a million, and searches for a famous
// title otherwise surface obscure shorts that happen to share the name.
const (
	wellKnownVotes = 25000
	knownVotes     = 1000
)

// rankByRating puts the best-reviewed films first, because that is nearly
// always the one being looked for — but only compares ratings that carry
// comparable weight. An exact title match wins over everything: someone
// searching "Dune" wants Dune, not its better-reviewed sequel.
func rankByRating(results []Result, query string) {
	want := strings.ToLower(strings.TrimSpace(query))
	sort.SliceStable(results, func(i, j int) bool {
		iExact := strings.ToLower(results[i].Title) == want
		jExact := strings.ToLower(results[j].Title) == want
		if iExact != jExact {
			return iExact
		}
		if a, b := results[i].credibility(), results[j].credibility(); a != b {
			return a > b
		}
		a, aok := results[i].rating()
		b, bok := results[j].rating()
		if aok != bok {
			return aok // unrated films sink below rated ones
		}
		return a > b
	})
}

// credibility buckets a film by how many people rated it.
func (r Result) credibility() int {
	switch {
	case r.RatingVotes >= wellKnownVotes:
		return 2
	case r.RatingVotes >= knownVotes:
		return 1
	default:
		return 0
	}
}

// rating parses the text rating for comparison.
func (r Result) rating() (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(r.Rating), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Detail resolves one candidate by id, used when the user picks a search hit
// so the stored record comes from the source rather than the browser.
func (s *Service) Detail(ctx context.Context, id string) (*Result, error) {
	id = strings.TrimSpace(id)
	if !s.Enabled() || id == "" {
		return nil, nil
	}
	if cached, ok := s.cache.get(id); ok {
		return &cached, nil
	}
	res, err := s.provider.Detail(ctx, id)
	if err == nil && res != nil {
		s.cache.put(id, *res)
	}
	return res, err
}

// Best returns the most likely match for a title, preferring an exact
// case-insensitive hit over the backend's own ordering. Used when someone
// types a title and submits without touching the search box.
func (s *Service) Best(ctx context.Context, query string) (*Result, error) {
	results, err := s.Search(ctx, query, 8)
	if err != nil || len(results) == 0 {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSpace(query))
	best := &results[0]
	for i := range results {
		if strings.ToLower(results[i].Title) == want {
			best = &results[i]
			break
		}
	}
	// Search results are thin (no plot or rating) on both backends, so fill
	// the winner in. A failure here is not fatal: we keep what we have.
	if full, err := s.Detail(ctx, best.detailID()); err == nil && full != nil {
		return full, nil
	}
	return best, nil
}

// detailID returns the id to hand back to the provider's Detail.
func (r Result) detailID() string {
	if r.Source == SourceTMDB && r.TMDBID > 0 {
		return fmt.Sprint(r.TMDBID)
	}
	return r.IMDbID
}

// Usage reports how much of the daily API allowance this run has spent. The
// backend tells us nothing about it, so this counts requests we sent.
func (s *Service) Usage() Usage {
	if !s.Enabled() {
		return Usage{}
	}
	return s.meter.snapshot(s.SourceLabel(), DailyRequestLimit)
}

// RecommendationsEnabled reports whether "films like this one" can be
// answered, which needs a TMDB key.
func (s *Service) RecommendationsEnabled() bool { return s != nil && s.tmdb != nil }

// Recommendations returns films similar to one we already have, looked up by
// TMDB id when we know it and by IMDb id otherwise, best-rated first. Returns
// nothing (without an error) when no TMDB key is configured.
func (s *Service) Recommendations(ctx context.Context, imdbID string, tmdbID int64, limit int) ([]Result, error) {
	if !s.RecommendationsEnabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}
	results, err := s.tmdb.Recommendations(ctx, imdbID, tmdbID, limit)
	if err != nil {
		return nil, err
	}
	rankByRating(results, "")
	return results, nil
}

// defaultHTTP is the client both providers use when none is supplied.
func defaultHTTP(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 8 * time.Second}
}

// naOr normalises the "N/A" sentinel that OMDb uses for missing fields.
func naOr(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "n/a") {
		return ""
	}
	return v
}
