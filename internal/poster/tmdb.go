package poster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// TMDBImageBase is TMDB's CDN prefix for the poster size we display.
const TMDBImageBase = "https://image.tmdb.org/t/p/w500"

// TMDB is the alternative backend, selected with CINEVOTE_POSTER_SOURCE=tmdb
// and a TMDB_API_KEY. It covers non-English titles better than OMDb.
type TMDB struct {
	apiKey string
	http   *http.Client
	base   string
}

func NewTMDB(apiKey string, hc *http.Client) *TMDB {
	return &TMDB{
		apiKey: strings.TrimSpace(apiKey),
		http:   defaultHTTP(hc),
		base:   "https://api.themoviedb.org/3",
	}
}

func (t *TMDB) Source() Source { return SourceTMDB }

type tmdbMovie struct {
	ID          int64   `json:"id"`
	IMDbID      string  `json:"imdb_id"`
	Title       string  `json:"title"`
	ReleaseDate string  `json:"release_date"`
	PosterPath  string  `json:"poster_path"`
	Overview    string  `json:"overview"`
	VoteAverage float64 `json:"vote_average"`
	VoteCount   int64   `json:"vote_count"`
	Runtime     int     `json:"runtime"`
	Genres      []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Credits struct {
		Cast []struct {
			Name string `json:"name"`
		} `json:"cast"`
		Crew []struct {
			Name string `json:"name"`
			Job  string `json:"job"`
		} `json:"crew"`
	} `json:"credits"`
}

func (t *TMDB) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	var body struct {
		Results []tmdbMovie `json:"results"`
	}
	if err := t.get(ctx, "/search/movie", url.Values{
		"query":         {query},
		"include_adult": {"false"},
	}, &body); err != nil {
		return nil, err
	}

	out := make([]Result, 0, limit)
	for _, m := range body.Results {
		if len(out) == limit {
			break
		}
		if strings.TrimSpace(m.Title) == "" {
			continue
		}
		out = append(out, t.result(m))
	}
	return out, nil
}

func (t *TMDB) Detail(ctx context.Context, id string) (*Result, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return nil, nil // not a TMDB id; nothing to look up
	}
	var m tmdbMovie
	// One request for the film and its credits, so we get a director and a few
	// names without a second round trip.
	if err := t.get(ctx, "/movie/"+id, url.Values{"append_to_response": {"credits"}}, &m); err != nil {
		return nil, err
	}
	if m.ID == 0 {
		return nil, nil
	}
	res := t.result(m)
	return &res, nil
}

func (t *TMDB) result(m tmdbMovie) Result {
	res := Result{
		Source:      SourceTMDB,
		TMDBID:      m.ID,
		IMDbID:      strings.TrimSpace(m.IMDbID),
		Title:       strings.TrimSpace(m.Title),
		Year:        firstYear(m.ReleaseDate),
		Overview:    strings.TrimSpace(m.Overview),
		RatingVotes: m.VoteCount,
	}
	if m.PosterPath != "" {
		res.PosterURL = TMDBImageBase + m.PosterPath
	}
	if m.VoteAverage > 0 {
		res.Rating = strconv.FormatFloat(m.VoteAverage, 'f', 1, 64)
	}
	if m.Runtime > 0 {
		res.Runtime = strconv.Itoa(m.Runtime) + " min"
	}
	names := make([]string, 0, len(m.Genres))
	for _, g := range m.Genres {
		names = append(names, g.Name)
	}
	res.Genres = strings.Join(names, ", ")

	for _, member := range m.Credits.Crew {
		if member.Job == "Director" {
			res.Director = member.Name
			break
		}
	}
	cast := make([]string, 0, 3)
	for _, actor := range m.Credits.Cast {
		if len(cast) == 3 {
			break
		}
		cast = append(cast, actor.Name)
	}
	res.Actors = strings.Join(cast, ", ")
	return res
}

// Recommendations returns films TMDB considers similar to the given one. A
// TMDB id is used directly; with only an IMDb id we resolve it first.
func (t *TMDB) Recommendations(ctx context.Context, imdbID string, tmdbID int64, limit int) ([]Result, error) {
	if tmdbID <= 0 {
		resolved, err := t.byIMDbID(ctx, imdbID)
		if err != nil || resolved == 0 {
			return nil, err
		}
		tmdbID = resolved
	}

	var body struct {
		Results []tmdbMovie `json:"results"`
	}
	path := "/movie/" + strconv.FormatInt(tmdbID, 10) + "/recommendations"
	if err := t.get(ctx, path, nil, &body); err != nil {
		return nil, err
	}

	out := make([]Result, 0, limit)
	for _, m := range body.Results {
		if len(out) == limit {
			break
		}
		if strings.TrimSpace(m.Title) == "" {
			continue
		}
		out = append(out, t.result(m))
	}
	return out, nil
}

// byIMDbID maps an IMDb id to TMDB's own id, which the recommendation endpoint
// requires. Returns 0 when TMDB does not know the film.
func (t *TMDB) byIMDbID(ctx context.Context, imdbID string) (int64, error) {
	imdbID = strings.TrimSpace(imdbID)
	if !strings.HasPrefix(imdbID, "tt") {
		return 0, nil
	}
	var body struct {
		MovieResults []struct {
			ID int64 `json:"id"`
		} `json:"movie_results"`
	}
	if err := t.get(ctx, "/find/"+imdbID, url.Values{"external_source": {"imdb_id"}}, &body); err != nil {
		return 0, err
	}
	if len(body.MovieResults) == 0 {
		return 0, nil
	}
	return body.MovieResults[0].ID, nil
}

func (t *TMDB) get(ctx context.Context, path string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("api_key", t.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("tmdb request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tmdb request: unexpected status %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("tmdb decode: %w", err)
	}
	return nil
}
