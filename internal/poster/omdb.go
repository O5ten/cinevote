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

// OMDb talks to omdbapi.com, the free IMDb data API. A key is a free signup at
// https://www.omdbapi.com/apikey.aspx and goes in OMDB_API_KEY.
type OMDb struct {
	apiKey string
	http   *http.Client
	base   string // overridable for tests
	meter  *meter // counts requests against the daily allowance
}

func NewOMDb(apiKey string, hc *http.Client) *OMDb {
	return &OMDb{
		apiKey: strings.TrimSpace(apiKey),
		http:   defaultHTTP(hc),
		base:   "https://www.omdbapi.com/",
	}
}

func (o *OMDb) Source() Source { return SourceIMDb }

// omdbSearch is the shape of the ?s= (search) response.
type omdbSearch struct {
	Search []struct {
		Title  string `json:"Title"`
		Year   string `json:"Year"`
		IMDbID string `json:"imdbID"`
		Type   string `json:"Type"`
		Poster string `json:"Poster"`
	} `json:"Search"`
	Response string `json:"Response"`
	Error    string `json:"Error"`
}

// omdbDetail is the shape of the ?i= / ?t= (single title) response.
type omdbDetail struct {
	Title      string `json:"Title"`
	Year       string `json:"Year"`
	Runtime    string `json:"Runtime"`
	Genre      string `json:"Genre"`
	Director   string `json:"Director"`
	Actors     string `json:"Actors"`
	Plot       string `json:"Plot"`
	Poster     string `json:"Poster"`
	IMDbRating string `json:"imdbRating"`
	IMDbVotes  string `json:"imdbVotes"`
	IMDbID     string `json:"imdbID"`
	Type       string `json:"Type"`
	Response   string `json:"Response"`
	Error      string `json:"Error"`
}

func (o *OMDb) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	var body omdbSearch
	if err := o.get(ctx, url.Values{
		"s":    {query},
		"type": {"movie"},
	}, &body); err != nil {
		return nil, err
	}
	// OMDb answers "False" for an empty result set as well as for real errors;
	// "not found" is not something the user needs to see as a failure.
	if !strings.EqualFold(body.Response, "True") {
		if err := o.apiError("search", body.Error); err != nil {
			return nil, err
		}
		return nil, nil
	}

	out := make([]Result, 0, limit)
	for _, r := range body.Search {
		if len(out) == limit {
			break
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			continue
		}
		out = append(out, Result{
			Source:    SourceIMDb,
			IMDbID:    strings.TrimSpace(r.IMDbID),
			Title:     title,
			Year:      firstYear(r.Year),
			PosterURL: naOr(r.Poster),
		})
	}
	return out, nil
}

func (o *OMDb) Detail(ctx context.Context, id string) (*Result, error) {
	params := url.Values{"plot": {"short"}}
	// Accept either an IMDb id or a raw title, so Best works even when a
	// search hit came back without an id.
	if strings.HasPrefix(id, "tt") {
		params.Set("i", id)
	} else {
		params.Set("t", id)
		params.Set("type", "movie")
	}

	var body omdbDetail
	if err := o.get(ctx, params, &body); err != nil {
		return nil, err
	}
	if !strings.EqualFold(body.Response, "True") {
		if err := o.apiError("detail", body.Error); err != nil {
			return nil, err
		}
		return nil, nil
	}

	return &Result{
		Source:      SourceIMDb,
		IMDbID:      strings.TrimSpace(body.IMDbID),
		Title:       strings.TrimSpace(body.Title),
		Year:        firstYear(body.Year),
		PosterURL:   naOr(body.Poster),
		Overview:    naOr(body.Plot),
		Rating:      naOr(body.IMDbRating),
		Runtime:     naOr(body.Runtime),
		Genres:      naOr(body.Genre),
		Director:    naOr(body.Director),
		Actors:      naOr(body.Actors),
		RatingVotes: parseVoteCount(body.IMDbVotes),
	}, nil
}

func (o *OMDb) get(ctx context.Context, params url.Values, out any) error {
	params.Set("apikey", o.apiKey)
	params.Set("r", "json")
	o.meter.record()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("omdb request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("omdb request: unexpected status %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("omdb decode: %w", err)
	}
	return nil
}

// apiError turns OMDb's "Response": "False" into an error, or nil when it just
// means "nothing found". Hitting the daily limit gets its own error so the UI
// can explain it rather than blaming the network.
func (o *OMDb) apiError(what, message string) error {
	if message == "" {
		return nil
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "not found"):
		return nil
	case strings.Contains(lower, "limit reached"), strings.Contains(lower, "request limit"):
		o.meter.markExhausted()
		return fmt.Errorf("omdb %s: %w", what, ErrQuotaExceeded)
	default:
		return fmt.Errorf("omdb %s: %s", what, message)
	}
}

// parseVoteCount reads OMDb's thousands-separated vote count ("2,600,123").
func parseVoteCount(raw string) int64 {
	raw = strings.ReplaceAll(naOr(raw), ",", "")
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// firstYear trims OMDb's range notation ("1999-2004", "2010–") to a single year.
func firstYear(raw string) string {
	raw = naOr(raw)
	for i, r := range raw {
		if r < '0' || r > '9' {
			return raw[:i]
		}
		if i == 3 {
			return raw[:4]
		}
	}
	return raw
}
