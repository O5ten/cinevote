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
	"strings"
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
type Service struct {
	provider Provider
}

// New builds the service. source may be "imdb", "tmdb" or "" (auto: IMDb when
// an OMDb key is present, otherwise TMDB). A missing key for the chosen
// backend disables lookups rather than failing startup — the app is perfectly
// usable with hand-pasted poster links.
func New(source, omdbKey, tmdbKey string) (*Service, error) {
	omdbKey, tmdbKey = strings.TrimSpace(omdbKey), strings.TrimSpace(tmdbKey)

	switch Source(strings.ToLower(strings.TrimSpace(source))) {
	case SourceIMDb:
		if omdbKey == "" {
			return &Service{}, nil
		}
		return &Service{provider: NewOMDb(omdbKey, nil)}, nil
	case SourceTMDB:
		if tmdbKey == "" {
			return &Service{}, nil
		}
		return &Service{provider: NewTMDB(tmdbKey, nil)}, nil
	case SourceNone:
		return &Service{}, nil
	case "":
		switch {
		case omdbKey != "":
			return &Service{provider: NewOMDb(omdbKey, nil)}, nil
		case tmdbKey != "":
			return &Service{provider: NewTMDB(tmdbKey, nil)}, nil
		}
		return &Service{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (use \"imdb\" or \"tmdb\")", ErrUnsupportedSource, source)
	}
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
	return s.provider.Search(ctx, query, limit)
}

// Detail resolves one candidate by id, used when the user picks a search hit
// so the stored record comes from the source rather than the browser.
func (s *Service) Detail(ctx context.Context, id string) (*Result, error) {
	id = strings.TrimSpace(id)
	if !s.Enabled() || id == "" {
		return nil, nil
	}
	return s.provider.Detail(ctx, id)
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
