package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/o5ten/cinevote/internal/auth"
	"github.com/o5ten/cinevote/internal/browse"
	"github.com/o5ten/cinevote/internal/poster"
	"github.com/o5ten/cinevote/internal/store"
)

// Board layouts. Cards are the default; the table is for scanning a long list.
const (
	ViewCards = "cards"
	ViewList  = "list"

	viewCookie = "cinevote_view"
)

const (
	maxTitleLen    = 200
	maxOverviewLen = 2000
	topCount       = 3 // how many movies get the prominent treatment
	searchLimit    = 8
	lookupTimeout  = 8 * time.Second
)

var yearRe = regexp.MustCompile(`^(18|19|20|21)\d{2}$`)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// notFound renders the error page for a request that routed fine but pointed
// at something that is not there.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request, heading, message string) {
	s.render(w, r, http.StatusNotFound, "error.html", map[string]any{
		"Heading": heading,
		"Message": message,
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	user, csrf := s.session(r)
	if user != nil {
		r = s.withUser(r, user, csrf)
	}
	s.render(w, r, http.StatusNotFound, "error.html", map[string]any{
		"Heading": "Sidan finns inte",
		"Message": "Länken leder ingenstans. Prova startsidan.",
	})
}

/* ------------------------------------------------------------- the board --- */

// boardView decides which layout to render. An explicit ?view= wins and is
// remembered, so the choice survives clicking around; otherwise the last choice
// applies, and failing that the card layout everyone starts on.
func (s *Server) boardView(w http.ResponseWriter, r *http.Request) string {
	if requested := r.URL.Query().Get("view"); requested != "" {
		view := ViewCards
		if requested == ViewList {
			view = ViewList
		}
		http.SetCookie(w, &http.Cookie{
			Name:     viewCookie,
			Value:    view,
			Path:     "/",
			MaxAge:   int((365 * 24 * time.Hour).Seconds()),
			HttpOnly: true,
			Secure:   s.cfg.SecureCookies,
			SameSite: http.SameSiteLaxMode,
		})
		return view
	}
	if c, err := r.Cookie(viewCookie); err == nil && c.Value == ViewList {
		return ViewList
	}
	return ViewCards
}

// viewURL rebuilds the current board URL with a different layout, so the toggle
// keeps whatever filters are active.
func viewURL(r *http.Request, view string) string {
	values := r.URL.Query()
	values.Set("view", view)
	return "/?" + values.Encode()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFrom(ctx)

	movies, err := s.store.Movies(ctx, user.ID)
	if err != nil {
		s.fail(w, r, "kunde inte hämta filmerna", err)
		return
	}
	used, err := s.store.VotesUsed(ctx, user.ID)
	if err != nil {
		s.fail(w, r, "kunde inte räkna dina röster", err)
		return
	}
	people, err := s.store.CountUsers(ctx)
	if err != nil {
		s.fail(w, r, "kunde inte räkna användarna", err)
		return
	}

	left := s.cfg.MaxVotes - used
	if left < 0 {
		left = 0
	}

	query := browse.ParseQuery(r.URL.Query())
	view := s.boardView(w, r)
	data := map[string]any{
		"Title":     "Filmröstning",
		"VotesUsed": used,
		"VotesLeft": left,
		"People":    people,
		"Query":     query,
		"Filtering": query.Active(),
		"View":      view,
		"IsList":    view == ViewList,
		"CardsURL":  viewURL(r, ViewCards),
		"ListURL":   viewURL(r, ViewList),
		// Action forms carry these so voting returns to the same view, filters
		// and scroll anchor instead of dumping you on a fresh board.
		"BackQuery": r.URL.RawQuery,
		"ReturnTo":  "/",
		// Dropdown contents come from the board itself, so they only ever
		// offer filters that can actually match something.
		"AllGenres":    browse.Genres(movies),
		"AllDirectors": browse.Directors(movies),
		"TotalMovies":  len(movies),
	}

	// A filtered board is one flat, sorted list: the podium is about the true
	// vote ranking, and showing it next to a filtered list would be a lie.
	if query.Active() {
		data["Results"] = browse.Apply(movies, query)
		s.render(w, r, http.StatusOK, "index.html", data)
		return
	}

	// Movies() sorts unseen-first by vote count, so slicing is enough. A movie
	// with no votes never gets the podium treatment, however few films exist.
	var top, rest, seen []store.Movie
	for _, m := range movies {
		switch {
		case m.Seen:
			seen = append(seen, m)
		case m.Rank <= topCount && m.Votes > 0:
			top = append(top, m)
		default:
			rest = append(rest, m)
		}
	}
	data["Top"] = top
	data["Rest"] = rest
	data["Seen"] = seen
	data["Winner"] = firstMovie(top)
	// The list layout wants one ranked table rather than podium plus grid.
	data["Open"] = append(append([]store.Movie{}, top...), rest...)
	s.render(w, r, http.StatusOK, "index.html", data)
}

func firstMovie(m []store.Movie) *store.Movie {
	if len(m) == 0 {
		return nil
	}
	return &m[0]
}

/* ----------------------------------------------------------------- auth --- */

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if user, _ := s.session(r); user != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Somebody who already gave the password only has to say who they are.
	if _, pending := s.pendingRole(r); pending && s.cfg.UseMattermost() {
		http.Redirect(w, r, "/jagar", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login.html", map[string]any{"Title": "Logga in"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// With Mattermost configured there are no per-person accounts: one shared
	// password, then pick yourself out of the chat directory.
	if s.cfg.UseMattermost() {
		s.handleSharedLogin(w, r)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if !s.logins.allow(clientIP(r)) {
		s.render(w, r, http.StatusTooManyRequests, "login.html", map[string]any{
			"Title": "Logga in",
			"Error": "För många försök. Vänta en stund och prova igen.",
			"Name":  username,
		})
		return
	}

	user, err := s.store.UserByUsername(r.Context(), username)
	if err != nil || !auth.CheckPassword(user.PasswordHash, password) {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("lookup user", "err", err)
		}
		s.render(w, r, http.StatusUnauthorized, "login.html", map[string]any{
			"Title": "Logga in",
			"Error": "Fel användarnamn eller lösenord.",
			"Name":  username,
		})
		return
	}

	s.logins.reset(clientIP(r))
	if err := s.startSession(w, r, user); err != nil {
		s.fail(w, r, "kunde inte logga in", err)
		return
	}
	s.setFlash(w, "ok", "Välkommen tillbaka, "+user.Username+"!")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	if user, _ := s.session(r); user != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "register.html", map[string]any{
		"Title":       "Skapa konto",
		"NeedsCode":   s.cfg.RegistrationCode != "",
		"MinPassword": auth.MinPasswordLen,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := r.FormValue("username")
	password := r.FormValue("password")
	code := strings.TrimSpace(r.FormValue("code"))

	reject := func(status int, msg string) {
		s.render(w, r, status, "register.html", map[string]any{
			"Title":       "Skapa konto",
			"Error":       msg,
			"Name":        strings.TrimSpace(username),
			"NeedsCode":   s.cfg.RegistrationCode != "",
			"MinPassword": auth.MinPasswordLen,
		})
	}

	if !s.logins.allow("register:" + clientIP(r)) {
		reject(http.StatusTooManyRequests, "För många försök. Vänta en stund och prova igen.")
		return
	}
	if s.cfg.RegistrationCode != "" && code != s.cfg.RegistrationCode {
		reject(http.StatusForbidden, "Fel inbjudningskod.")
		return
	}

	name, err := auth.ValidateUsername(username)
	if err != nil {
		reject(http.StatusBadRequest, err.Error())
		return
	}
	if strings.EqualFold(name, s.cfg.AdminUsername) {
		reject(http.StatusConflict, "Det användarnamnet är reserverat.")
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		reject(http.StatusBadRequest, err.Error())
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.fail(w, r, "kunde inte skapa kontot", err)
		return
	}
	user, err := s.store.CreateUser(ctx, name, hash, false)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateUser) {
			reject(http.StatusConflict, "Användarnamnet är taget.")
			return
		}
		s.fail(w, r, "kunde inte skapa kontot", err)
		return
	}

	if err := s.startSession(w, r, user); err != nil {
		s.fail(w, r, "kontot skapades men inloggningen misslyckades", err)
		return
	}
	s.setFlash(w, "ok", "Kontot är skapat. Du har "+plural(s.cfg.MaxVotes)+" att fördela!")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func plural(n int) string {
	if n == 1 {
		return "1 röst"
	}
	return itoa(n) + " röster"
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.endSession(w, r)
	s.clearPending(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// endSession forgets the session both server- and browser-side.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.store.DeleteSession(r.Context(), c.Value); err != nil {
			s.log.Error("delete session", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// startSession opens a session for an account whose own flag decides whether it
// is the admin — CineVote's own login.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user *store.User) error {
	return s.startSessionAs(w, r, user, "")
}

// startSessionAs opens a session, optionally carrying a role granted by the
// password rather than by the account. That is how Mattermost mode has an
// admin without an admin account.
func (s *Server) startSessionAs(w http.ResponseWriter, r *http.Request, user *store.User, role string) error {
	token, err := auth.Token()
	if err != nil {
		return err
	}
	csrf, err := auth.Token()
	if err != nil {
		return err
	}
	expires := time.Now().Add(s.cfg.SessionTTL)
	if err := s.store.CreateSession(r.Context(), user.ID, token, csrf, role, expires); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

/* --------------------------------------------------------------- movies --- */

func (s *Server) handleAddMovie(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFrom(ctx)

	title := strings.TrimSpace(r.FormValue("title"))
	year := strings.TrimSpace(r.FormValue("year"))
	posterURL := strings.TrimSpace(r.FormValue("poster_url"))
	overview := strings.TrimSpace(r.FormValue("overview"))
	// source_id is filled in by the search box: an IMDb id from OMDb
	// ("tt0083658") or a numeric TMDB id.
	sourceID := strings.TrimSpace(r.FormValue("source_id"))

	switch {
	case title == "":
		s.redirectFlash(w, r, "error", "Filmen behöver en titel.")
		return
	case utf8.RuneCountInString(title) > maxTitleLen:
		s.redirectFlash(w, r, "error", "Titeln är för lång.")
		return
	case year != "" && !yearRe.MatchString(year):
		s.redirectFlash(w, r, "error", "Årtalet ser inte ut som ett årtal.")
		return
	case posterURL != "" && !validPosterURL(posterURL):
		s.redirectFlash(w, r, "error", "Posterlänken måste vara en http- eller https-adress.")
		return
	case len(sourceID) > 32:
		s.redirectFlash(w, r, "error", "Ogiltig filmreferens.")
		return
	}
	if utf8.RuneCountInString(overview) > maxOverviewLen {
		overview = string([]rune(overview)[:maxOverviewLen])
	}

	// Look the film up so poster, rating, director and runtime come from OMDb
	// rather than from the browser: by id when the user picked a search hit, by
	// title otherwise. Failure here is not fatal — the suggestion still goes in.
	meta := s.lookup(ctx, sourceID, title)

	newMovie := store.NewMovie{
		Title:       title,
		Year:        year,
		PosterURL:   posterURL,
		Overview:    overview,
		SuggestedBy: user.ID,
	}
	if meta != nil {
		newMovie.IMDbID = meta.IMDbID
		newMovie.TMDBID = meta.TMDBID
		newMovie.Rating = meta.Rating
		newMovie.Runtime = meta.Runtime
		newMovie.Genres = meta.Genres
		newMovie.Director = meta.Director
		newMovie.Actors = meta.Actors
		// The typed title wins (someone may prefer the Swedish one), but
		// anything the user left blank gets filled in.
		if newMovie.PosterURL == "" {
			newMovie.PosterURL = meta.PosterURL
		}
		if newMovie.Year == "" {
			newMovie.Year = meta.Year
		}
		if newMovie.Overview == "" {
			newMovie.Overview = meta.Overview
		}
	}

	id, err := s.store.AddMovie(ctx, newMovie)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateFilm) {
			s.redirectFlash(w, r, "error", title+" ligger redan på listan.")
			return
		}
		s.fail(w, r, "kunde inte lägga till filmen", err)
		return
	}
	s.redirectToMovie(w, r, "ok", title+" är tillagd. Glöm inte att rösta!", id)
}

// lookup resolves metadata for a new suggestion, preferring the id the search
// box supplied and falling back to the title. Whatever the user typed still
// wins — the caller only fills in the fields they left blank.
func (s *Server) lookup(ctx context.Context, sourceID, title string) *poster.Result {
	if !s.posters.Enabled() {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	if sourceID != "" {
		res, err := s.posters.Detail(lookupCtx, sourceID)
		if err != nil {
			s.log.Warn("metadata lookup by id failed", "id", sourceID, "err", err)
			return nil
		}
		return res
	}
	res, err := s.posters.Best(lookupCtx, title)
	if err != nil {
		s.log.Warn("metadata lookup by title failed", "title", title, "err", err)
		return nil
	}
	return res
}

func (s *Server) handleVote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFrom(ctx)
	id, err := pathID(r)
	if err != nil {
		s.redirectFlash(w, r, "error", "Okänd film.")
		return
	}

	switch err := s.store.Vote(ctx, user.ID, id); {
	case err == nil:
		left, _ := s.store.VotesLeft(ctx, user.ID)
		s.redirectToMovie(w, r, "ok", "Röst lagd. "+plural(left)+" kvar.", id)
	case errors.Is(err, store.ErrNoVotesLeft):
		s.redirectToMovie(w, r, "error", "Dina röster är slut. Ta tillbaka en röst först.", id)
	case errors.Is(err, store.ErrAlreadyVoted):
		s.redirectToMovie(w, r, "error", "Du har redan röstat på den filmen.", id)
	case errors.Is(err, store.ErrMovieSeen):
		s.redirectToMovie(w, r, "error", "Filmen är redan sedd.", id)
	case errors.Is(err, store.ErrNotFound):
		s.redirectFlash(w, r, "error", "Filmen finns inte längre.")
	default:
		s.fail(w, r, "kunde inte lägga rösten", err)
	}
}

func (s *Server) handleUnvote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFrom(ctx)
	id, err := pathID(r)
	if err != nil {
		s.redirectFlash(w, r, "error", "Okänd film.")
		return
	}

	switch err := s.store.Unvote(ctx, user.ID, id); {
	case err == nil:
		left, _ := s.store.VotesLeft(ctx, user.ID)
		s.redirectToMovie(w, r, "ok", "Rösten är tillbaka. Du har "+plural(left)+".", id)
	case errors.Is(err, store.ErrNotFound):
		s.redirectToMovie(w, r, "error", "Ingen röst att ta tillbaka där.", id)
	default:
		s.fail(w, r, "kunde inte ta tillbaka rösten", err)
	}
}

// handleSetSeen is the admin's "we watched it" switch. Everyone who voted for
// the movie gets their vote back for the next round.
func (s *Server) handleSetSeen(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.redirectFlash(w, r, "error", "Okänd film.")
		return
	}
	seen := r.FormValue("seen") != "0"

	movie, err := s.store.MovieByID(r.Context(), id)
	if err != nil {
		s.redirectFlash(w, r, "error", "Filmen finns inte längre.")
		return
	}
	if err := s.store.SetSeen(r.Context(), id, seen); err != nil {
		s.fail(w, r, "kunde inte uppdatera filmen", err)
		return
	}

	if seen {
		s.redirectToMovie(w, r, "ok", movie.Title+" är markerad som sedd. "+
			plural(movie.Votes)+" tillbaka till "+voters(movie.Votes)+".", id)
	} else {
		s.redirectToMovie(w, r, "ok", movie.Title+" är tillbaka i röstningen.", id)
	}
}

func voters(n int) string {
	if n == 1 {
		return "en röstande"
	}
	return "de röstande"
}

// handleDeleteMovie lets the admin remove anything, and lets a user withdraw
// their own suggestion as long as nobody has voted for it yet.
func (s *Server) handleDeleteMovie(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFrom(ctx)
	id, err := pathID(r)
	if err != nil {
		s.redirectFlash(w, r, "error", "Okänd film.")
		return
	}

	movie, err := s.store.MovieByID(ctx, id)
	if err != nil {
		s.redirectFlash(w, r, "error", "Filmen finns inte längre.")
		return
	}
	own := movie.SuggestedBy != "" && strings.EqualFold(movie.SuggestedBy, user.Username)
	if !user.IsAdmin {
		if !own {
			s.redirectFlash(w, r, "error", "Du kan bara ta bort dina egna förslag.")
			return
		}
		if movie.Votes > 0 {
			s.redirectFlash(w, r, "error", "Filmen har röster – be admin ta bort den.")
			return
		}
	}
	if err := s.store.DeleteMovie(ctx, id); err != nil {
		s.fail(w, r, "kunde inte ta bort filmen", err)
		return
	}
	s.redirectFlash(w, r, "ok", movie.Title+" är borttagen.")
}

/* -------------------------------------------------------------- similar --- */

// handleSimilar answers "what else is like this one?" in two parts: the films
// already on the board that resemble it, and — when a TMDB key is configured —
// recommendations for films nobody has suggested yet.
func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFrom(ctx)

	id, err := pathID(r)
	if err != nil {
		s.notFound(w, r, "Okänd film", "Länken pekar på en film som inte finns.")
		return
	}

	movies, err := s.store.Movies(ctx, user.ID)
	if err != nil {
		s.fail(w, r, "kunde inte hämta filmerna", err)
		return
	}
	// Take the target from the same list, so it carries the viewer's own vote
	// state and its current rank.
	var target *store.Movie
	for i := range movies {
		if movies[i].ID == id {
			target = &movies[i]
			break
		}
	}
	if target == nil {
		s.notFound(w, r, "Filmen finns inte", "Någon kan ha tagit bort den.")
		return
	}

	left, err := s.store.VotesLeft(ctx, user.ID)
	if err != nil {
		s.fail(w, r, "kunde inte räkna dina röster", err)
		return
	}

	data := map[string]any{
		"Title":      "Liknande " + target.Title,
		"Movie":      *target,
		"Matches":    browse.Similar(*target, movies, 6),
		"VotesLeft":  left,
		"TMDBKeyURL": TMDBKeyURL,
	}

	if s.posters.RecommendationsEnabled() {
		recCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()

		found, err := s.posters.Recommendations(recCtx, target.IMDbID, target.TMDBID, 12)
		if err != nil {
			s.log.Warn("recommendation lookup failed", "movie", target.Title, "err", err)
			data["RecommendError"] = true
		} else {
			data["Recommendations"] = withoutKnown(found, movies, 8)
		}
	}

	s.render(w, r, http.StatusOK, "similar.html", data)
}

// withoutKnown drops recommendations that are already on the board — suggesting
// a film that is right there would be noise.
func withoutKnown(found []poster.Result, known []store.Movie, limit int) []poster.Result {
	byIMDb := map[string]bool{}
	byTitle := map[string]bool{}
	for _, m := range known {
		if m.IMDbID != "" {
			byIMDb[m.IMDbID] = true
		}
		byTitle[strings.ToLower(m.Title)] = true
	}

	out := make([]poster.Result, 0, limit)
	for _, res := range found {
		if len(out) == limit {
			break
		}
		if (res.IMDbID != "" && byIMDb[res.IMDbID]) || byTitle[strings.ToLower(res.Title)] {
			continue
		}
		out = append(out, res)
	}
	return out
}

/* ---------------------------------------------------------------- admin --- */

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := s.store.UserStats(ctx)
	if err != nil {
		s.fail(w, r, "kunde inte hämta användarna", err)
		return
	}
	movies, err := s.store.Movies(ctx, userFrom(ctx).ID)
	if err != nil {
		s.fail(w, r, "kunde inte hämta filmerna", err)
		return
	}
	// The header shows the admin's own vote budget too.
	left, err := s.store.VotesLeft(ctx, userFrom(ctx).ID)
	if err != nil {
		s.fail(w, r, "kunde inte räkna dina röster", err)
		return
	}
	s.render(w, r, http.StatusOK, "admin.html", map[string]any{
		"Title":     "Admin",
		"Users":     users,
		"Movies":    movies,
		"VotesLeft": left,
		"ReturnTo":  "/admin",
	})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.adminFlash(w, r, "error", "Okänd användare.")
		return
	}
	if id == userFrom(r.Context()).ID {
		s.adminFlash(w, r, "error", "Du kan inte ta bort dig själv.")
		return
	}
	switch err := s.store.DeleteUser(r.Context(), id); {
	case err == nil:
		s.adminFlash(w, r, "ok", "Användaren är borttagen.")
	case errors.Is(err, store.ErrNotFound):
		s.adminFlash(w, r, "error", "Användaren finns inte, eller är admin.")
	default:
		s.fail(w, r, "kunde inte ta bort användaren", err)
	}
}

/* ----------------------------------------------------------- poster api --- */

// handleSearch backs the "sök film" box in the suggestion form. It proxies the
// query so the API key never reaches the browser.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	results := []poster.Result{}

	if s.posters.Enabled() && query != "" {
		ctx, cancel := context.WithTimeout(r.Context(), lookupTimeout)
		defer cancel()
		found, err := s.posters.Search(ctx, query, searchLimit)
		if err != nil {
			s.log.Warn("metadata search failed", "query", query, "err", err)
			if errors.Is(err, poster.ErrQuotaExceeded) {
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"error": "Dagens " + itoa(poster.DailyRequestLimit) + " anrop till " +
						s.posters.SourceLabel() + " är slut. Fyll i filmen manuellt tills imorgon.",
					"usage": s.posters.Usage(),
				})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": "Kunde inte nå " + s.posters.SourceLabel() + " just nu. Fyll i titeln manuellt.",
				"usage": s.posters.Usage(),
			})
			return
		}
		results = append(results, found...)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.posters.Enabled(),
		"source":  s.posters.SourceLabel(),
		"results": results,
		// So the page can keep the quota counter current without a reload.
		"usage": s.posters.Usage(),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return // client hung up
	}
}

/* -------------------------------------------------------------- helpers --- */

// redirectFlash is the post/redirect/get tail every form action shares. It
// returns to the page the form was on, with no anchor.
func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, level, msg string) {
	s.setFlash(w, level, msg)
	http.Redirect(w, r, returnTarget(r), http.StatusSeeOther)
}

// redirectToMovie is the same, but anchored to one film. Voting used to send
// the reader back to the top of a long board; landing on the card they just
// acted on keeps their place, and works without JavaScript.
func (s *Server) redirectToMovie(w http.ResponseWriter, r *http.Request, level, msg string, movieID int64) {
	s.setFlash(w, level, msg)
	http.Redirect(w, r, returnTarget(r)+"#"+movieAnchor(movieID), http.StatusSeeOther)
}

func (s *Server) adminFlash(w http.ResponseWriter, r *http.Request, level, msg string) {
	s.setFlash(w, level, msg)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// movieAnchor is the element id a film's card and admin row carry.
func movieAnchor(movieID int64) string {
	return "film-" + strconv.FormatInt(movieID, 10)
}

// boardParams are the query parameters a form may ask to be carried back.
// Anything else in a submitted "back" value is dropped.
var boardParams = []string{"q", "genre", "director", "min_rating", "sort", "show", "view"}

// similarPath matches the one other page that hosts vote buttons.
var similarPath = regexp.MustCompile(`^/movies/[0-9]{1,18}/similar$`)

// returnTarget is the page a form wants to go back to, rebuilt from an
// allowlist. Voting should return you to the same view, filtered the same way —
// but a crafted "return" or "back" value must not be able to bounce anyone
// somewhere else, so nothing is echoed back verbatim.
func returnTarget(r *http.Request) string {
	base := "/"
	switch requested := r.FormValue("return"); {
	case requested == "/admin":
		base = "/admin"
	case similarPath.MatchString(requested):
		base = requested
	}

	raw := strings.TrimPrefix(r.FormValue("back"), "?")
	if raw == "" {
		return base
	}
	submitted, err := url.ParseQuery(raw)
	if err != nil {
		return base
	}
	keep := url.Values{}
	for _, key := range boardParams {
		if v := strings.TrimSpace(submitted.Get(key)); v != "" {
			keep.Set(key, v)
		}
	}
	if len(keep) == 0 {
		return base
	}
	return base + "?" + keep.Encode()
}

// fail logs the real error and shows the user a short explanation.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, msg string, err error) {
	s.log.Error(msg, "err", err, "path", r.URL.Path)
	s.render(w, r, http.StatusInternalServerError, "error.html", map[string]any{
		"Heading": "Något gick fel",
		"Message": "Vi " + msg + ". Prova igen om en stund.",
	})
}

func parseInt64(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func itoa(n int) string { return strconv.Itoa(n) }

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func validPosterURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
