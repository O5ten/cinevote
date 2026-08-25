// Package store owns all persistence for CineVote: users, sessions, movie
// suggestions and votes. It is deliberately the only place that knows SQL.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultMaxVotes is the number of votes each user may spread over the
// unseen movies. One vote per movie, so this is also the max number of
// movies a user can back at once.
const DefaultMaxVotes = 5

var (
	ErrNotFound      = errors.New("not found")
	ErrDuplicateUser = errors.New("username already taken")
	ErrDuplicateFilm = errors.New("movie already suggested")
	ErrNoVotesLeft   = errors.New("no votes left")
	ErrAlreadyVoted  = errors.New("already voted for this movie")
	ErrMovieSeen     = errors.New("movie is already marked as seen")
)

type Store struct {
	db *sql.DB
	// MaxVotes is the per-user vote budget. Votes on movies that have been
	// marked as seen do not count against it.
	MaxVotes int
}

// Open opens (and migrates) the SQLite database at path. Use ":memory:" for
// tests. The pragmas matter: WAL keeps readers from blocking the writer and
// busy_timeout stops "database is locked" under concurrent votes.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite takes a single writer; keeping the pool small avoids lock churn.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	s := &Store{db: db, MaxVotes: DefaultMaxVotes}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT    NOT NULL,
	username_ci   TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	-- Set when the account is identified by a Mattermost user rather than by a
	-- password of its own. display_name is what the pages show.
	mm_username   TEXT    NOT NULL DEFAULT '',
	mm_user_id    TEXT    NOT NULL DEFAULT '',
	display_name  TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf       TEXT    NOT NULL,
	expires_at INTEGER NOT NULL,
	-- In Mattermost mode the password decides the role, not the account, so
	-- the session carries it: "admin" or "" for an ordinary member.
	role       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
-- One CineVote account per chat account. Partial, so password accounts (which
-- have no chat account) do not collide on the empty string.
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_mm ON users(mm_username) WHERE mm_username <> '';

CREATE TABLE IF NOT EXISTS movies (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	title        TEXT    NOT NULL,
	title_ci     TEXT    NOT NULL,
	year         TEXT    NOT NULL DEFAULT '',
	poster_url   TEXT    NOT NULL DEFAULT '',
	overview     TEXT    NOT NULL DEFAULT '',
	imdb_id      TEXT    NOT NULL DEFAULT '',
	tmdb_id      INTEGER,
	rating       TEXT    NOT NULL DEFAULT '',
	runtime      TEXT    NOT NULL DEFAULT '',
	genres       TEXT    NOT NULL DEFAULT '',
	director     TEXT    NOT NULL DEFAULT '',
	actors       TEXT    NOT NULL DEFAULT '',
	suggested_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
	seen         INTEGER NOT NULL DEFAULT 0,
	seen_at      INTEGER,
	created_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_movies_title_year ON movies(title_ci, year);

CREATE TABLE IF NOT EXISTS votes (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	movie_id   INTEGER NOT NULL REFERENCES movies(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, movie_id)
);
CREATE INDEX IF NOT EXISTS idx_votes_movie ON votes(movie_id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS leaves an older table alone, so columns added
	// in later versions have to be filled in separately.
	if err := s.ensureColumns("users", []column{
		{"mm_username", "TEXT NOT NULL DEFAULT ''"},
		{"mm_user_id", "TEXT NOT NULL DEFAULT ''"},
		{"display_name", "TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}
	if err := s.ensureColumns("sessions", []column{
		{"role", "TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}
	// The index needs the column, so it cannot live in the schema above for a
	// database that predates it.
	if _, err := s.db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_mm ON users(mm_username) WHERE mm_username <> ''`); err != nil {
		return fmt.Errorf("create mattermost index: %w", err)
	}
	return s.ensureColumns("movies", []column{
		{"imdb_id", "TEXT NOT NULL DEFAULT ''"},
		{"rating", "TEXT NOT NULL DEFAULT ''"},
		{"runtime", "TEXT NOT NULL DEFAULT ''"},
		{"genres", "TEXT NOT NULL DEFAULT ''"},
		{"director", "TEXT NOT NULL DEFAULT ''"},
		{"actors", "TEXT NOT NULL DEFAULT ''"},
	})
}

type column struct {
	name string
	decl string
}

// ensureColumns adds any of the given columns that the table is missing. SQLite
// has no "ADD COLUMN IF NOT EXISTS", so we ask what is there first.
func (s *Store) ensureColumns(table string, want []column) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()

	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan column of %s: %w", table, err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, col := range want {
		if have[col.name] {
			continue
		}
		// Identifiers come from the constant list above, never from input.
		if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + col.name + " " + col.decl); err != nil {
			return fmt.Errorf("add column %s.%s: %w", table, col.name, err)
		}
	}
	return nil
}

/* ---------------------------------------------------------------- users --- */

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time

	// Set for accounts identified through Mattermost.
	MMUsername  string
	MMUserID    string
	DisplayName string
}

// Name is what the pages show: the display name when there is one, otherwise
// the username.
func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// FromMattermost reports whether the account is identified by a chat account
// rather than a password of its own.
func (u User) FromMattermost() bool { return u.MMUsername != "" }

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, isAdmin bool) (*User, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, username_ci, password_hash, is_admin, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		username, ci(username), passwordHash, boolToInt(isAdmin), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateUser
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, PasswordHash: passwordHash, IsAdmin: isAdmin, CreatedAt: now}, nil
}

const userColumns = `id, username, password_hash, is_admin, created_at,
	mm_username, mm_user_id, display_name`

func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username_ci = ?`, ci(username)))
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// UserByMattermost finds the account belonging to a chat username.
func (s *Store) UserByMattermost(ctx context.Context, mmUsername string) (*User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE mm_username = ?`, ci(mmUsername)))
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	var admin int
	var created int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &created,
		&u.MMUsername, &u.MMUserID, &u.DisplayName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan user: %w", err)
	}
	u.IsAdmin = admin != 0
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

// UpsertMattermostUser finds or creates the CineVote account for a chat
// account, keeping the shown name in step with the chat. The account has no
// password: the shared one already got them in, and the chat account is what
// says who they are.
func (s *Store) UpsertMattermostUser(ctx context.Context, mmUsername, mmUserID, displayName string) (*User, error) {
	mmUsername = ci(mmUsername)
	if mmUsername == "" {
		return nil, fmt.Errorf("no mattermost username")
	}
	if displayName == "" {
		displayName = mmUsername
	}

	if _, err := s.db.ExecContext(ctx, `
INSERT INTO users (username, username_ci, password_hash, is_admin, created_at,
                   mm_username, mm_user_id, display_name)
VALUES (?, ?, '', 0, ?, ?, ?, ?)
-- The unique index on mm_username is partial, so the conflict target has to
-- repeat its predicate for SQLite to match them up.
ON CONFLICT(mm_username) WHERE mm_username <> '' DO UPDATE SET
	mm_user_id   = excluded.mm_user_id,
	display_name = excluded.display_name`,
		mmUsername, mmUsername, time.Now().Unix(), mmUsername, mmUserID, displayName); err != nil {
		if isUniqueViolation(err) {
			// A password account already holds that username. Nothing sensible
			// to merge, so say so rather than hijack it.
			return nil, ErrDuplicateUser
		}
		return nil, fmt.Errorf("upsert mattermost user: %w", err)
	}
	return s.UserByMattermost(ctx, mmUsername)
}

// UpsertAdmin creates or updates the single admin account and demotes anyone
// else who happens to carry the flag. The requirement list asks for exactly
// one admin, so we enforce that here rather than trusting callers.
func (s *Store) UpsertAdmin(ctx context.Context, username, passwordHash string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, username_ci, password_hash, is_admin, created_at)
		 VALUES (?, ?, ?, 1, ?)
		 ON CONFLICT(username_ci) DO UPDATE SET password_hash = excluded.password_hash, is_admin = 1`,
		username, ci(username), passwordHash, time.Now().Unix()); err != nil {
		return nil, fmt.Errorf("upsert admin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET is_admin = 0 WHERE username_ci <> ?`, ci(username)); err != nil {
		return nil, fmt.Errorf("demote others: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.UserByUsername(ctx, username)
}

// RoleAdmin is the session role that grants admin rights in Mattermost mode.
const RoleAdmin = "admin"

// UserStat is a row for the admin user list.
type UserStat struct {
	User
	VotesUsed int
	Movies    int
}

func (s *Store) UserStats(ctx context.Context) ([]UserStat, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT u.id, u.username, u.is_admin, u.created_at, u.mm_username, u.display_name,
       (SELECT COUNT(*) FROM votes v JOIN movies m ON m.id = v.movie_id
         WHERE v.user_id = u.id AND m.seen = 0) AS votes_used,
       (SELECT COUNT(*) FROM movies m2 WHERE m2.suggested_by = u.id)  AS suggested
  FROM users u
 ORDER BY u.is_admin DESC, u.username_ci ASC`)
	if err != nil {
		return nil, fmt.Errorf("user stats: %w", err)
	}
	defer rows.Close()

	var out []UserStat
	for rows.Next() {
		var st UserStat
		var admin int
		var created int64
		if err := rows.Scan(&st.ID, &st.Username, &admin, &created,
			&st.MMUsername, &st.DisplayName, &st.VotesUsed, &st.Movies); err != nil {
			return nil, fmt.Errorf("scan user stat: %w", err)
		}
		st.IsAdmin = admin != 0
		st.CreatedAt = time.Unix(created, 0)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// DeleteUser removes a user; their votes go with them (ON DELETE CASCADE) and
// their suggestions stay but lose the attribution.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND is_admin = 0`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

/* ------------------------------------------------------------- sessions --- */

// CreateSession opens a session. role is "admin" when the admin password was
// used in Mattermost mode, and empty otherwise — an account's own is_admin
// flag still applies on top of it.
func (s *Store) CreateSession(ctx context.Context, userID int64, token, csrf, role string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token, user_id, csrf, expires_at, role) VALUES (?, ?, ?, ?, ?)`,
		token, userID, csrf, expires.Unix(), role)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Session resolves a cookie token to its user, ignoring expired rows. The
// returned user carries the session's role folded into IsAdmin, so callers do
// not have to remember which mode they are in.
func (s *Store) Session(ctx context.Context, token string) (*User, string, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT u.id, u.username, u.password_hash, u.is_admin, u.created_at,
       u.mm_username, u.mm_user_id, u.display_name, s.csrf, s.role
  FROM sessions s JOIN users u ON u.id = s.user_id
 WHERE s.token = ? AND s.expires_at > ?`, token, time.Now().Unix())

	var u User
	var admin int
	var created int64
	var csrf, role string
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &created,
		&u.MMUsername, &u.MMUserID, &u.DisplayName, &csrf, &role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("scan session: %w", err)
	}
	u.IsAdmin = admin != 0 || role == RoleAdmin
	u.CreatedAt = time.Unix(created, 0)
	return &u, csrf, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

/* --------------------------------------------------------------- movies --- */

type Movie struct {
	ID          int64
	Title       string
	Year        string
	PosterURL   string
	Overview    string
	IMDbID      string // e.g. "tt0083658", from OMDb
	TMDBID      int64
	Rating      string // IMDb rating as text, e.g. "8.1"
	Runtime     string // e.g. "117 min"
	Genres      string // comma separated
	Director    string
	Actors      string // comma separated, a few headline names
	SuggestedBy string // username, empty if the account is gone
	Seen        bool
	SeenAt      time.Time
	CreatedAt   time.Time

	Votes     int      // number of people backing it
	VotedByMe bool     // for the viewing user
	Voters    []string // usernames, for the tooltip / admin view
	Rank      int      // 1-based rank among unseen movies, by votes
}

// IMDbURL is the public IMDb page, or "" when the movie was added by hand.
func (m Movie) IMDbURL() string {
	if m.IMDbID == "" {
		return ""
	}
	return "https://www.imdb.com/title/" + m.IMDbID + "/"
}

type NewMovie struct {
	Title       string
	Year        string
	PosterURL   string
	Overview    string
	IMDbID      string
	TMDBID      int64
	Rating      string
	Runtime     string
	Genres      string
	Director    string
	Actors      string
	SuggestedBy int64
}

func (s *Store) AddMovie(ctx context.Context, m NewMovie) (int64, error) {
	var tmdb any
	if m.TMDBID > 0 {
		tmdb = m.TMDBID
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO movies (title, title_ci, year, poster_url, overview, imdb_id, tmdb_id,
                    rating, runtime, genres, director, actors, suggested_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Title, ci(m.Title), m.Year, m.PosterURL, m.Overview, m.IMDbID, tmdb,
		m.Rating, m.Runtime, m.Genres, m.Director, m.Actors, m.SuggestedBy, time.Now().Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDuplicateFilm
		}
		return 0, fmt.Errorf("add movie: %w", err)
	}
	return res.LastInsertId()
}

// Movies returns every movie ordered for display, which is also the order that
// decides what gets premiered: unseen first, then most votes, then — for films
// on equal votes — the highest rated, then the most recently released, and
// finally the earliest suggested so the order is always stable. Rank is filled
// in for unseen movies, which is what the "top 3" section keys off.
func (s *Store) Movies(ctx context.Context, viewerID int64) ([]Movie, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.title, m.year, m.poster_url, m.overview,
       m.imdb_id, COALESCE(m.tmdb_id, 0), m.rating, m.runtime, m.genres,
       m.director, m.actors, COALESCE(NULLIF(u.display_name, ''), u.username, ''),
       m.seen, COALESCE(m.seen_at, 0), m.created_at,
       (SELECT COUNT(*) FROM votes v WHERE v.movie_id = m.id) AS votes,
       EXISTS(SELECT 1 FROM votes v2 WHERE v2.movie_id = m.id AND v2.user_id = ?) AS voted_by_me,
       COALESCE((SELECT GROUP_CONCAT(COALESCE(NULLIF(vu.display_name, ''), vu.username), ', ')
                   FROM votes v3 JOIN users vu ON vu.id = v3.user_id
                  WHERE v3.movie_id = m.id), '') AS voters
  FROM movies m
  LEFT JOIN users u ON u.id = m.suggested_by
 ORDER BY m.seen ASC,
          votes DESC,
          -- Equal support is broken by the better film: highest rating first,
          -- then the most recently released. Both are stored as text, so they
          -- are cast; an empty one becomes NULL, which SQLite sorts last in a
          -- DESC order, so an unrated or undated film loses the tie-break.
          CAST(NULLIF(m.rating, '') AS REAL) DESC,
          CAST(NULLIF(m.year, '') AS INTEGER) DESC,
          -- Last resort, so the order never wobbles between requests.
          m.created_at ASC`, viewerID)
	if err != nil {
		return nil, fmt.Errorf("list movies: %w", err)
	}
	defer rows.Close()

	var out []Movie
	rank := 0
	for rows.Next() {
		var m Movie
		var seen, voted int
		var seenAt, created int64
		var voters string
		if err := rows.Scan(&m.ID, &m.Title, &m.Year, &m.PosterURL, &m.Overview,
			&m.IMDbID, &m.TMDBID, &m.Rating, &m.Runtime, &m.Genres,
			&m.Director, &m.Actors,
			&m.SuggestedBy, &seen, &seenAt, &created, &m.Votes, &voted, &voters); err != nil {
			return nil, fmt.Errorf("scan movie: %w", err)
		}
		m.Seen = seen != 0
		m.VotedByMe = voted != 0
		if seenAt > 0 {
			m.SeenAt = time.Unix(seenAt, 0)
		}
		m.CreatedAt = time.Unix(created, 0)
		if voters != "" {
			m.Voters = strings.Split(voters, ", ")
		}
		if !m.Seen {
			rank++
			m.Rank = rank
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) MovieByID(ctx context.Context, id int64) (*Movie, error) {
	var m Movie
	var seen int
	var seenAt, created int64
	err := s.db.QueryRowContext(ctx, `
SELECT m.id, m.title, m.year, m.poster_url, m.overview,
       m.imdb_id, COALESCE(m.tmdb_id, 0), m.rating, m.runtime, m.genres,
       m.director, m.actors,
       COALESCE(NULLIF(u.display_name, ''), u.username, ''), m.seen, COALESCE(m.seen_at, 0), m.created_at,
       (SELECT COUNT(*) FROM votes v WHERE v.movie_id = m.id)
  FROM movies m LEFT JOIN users u ON u.id = m.suggested_by
 WHERE m.id = ?`, id).
		Scan(&m.ID, &m.Title, &m.Year, &m.PosterURL, &m.Overview,
			&m.IMDbID, &m.TMDBID, &m.Rating, &m.Runtime, &m.Genres,
			&m.Director, &m.Actors,
			&m.SuggestedBy, &seen, &seenAt, &created, &m.Votes)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get movie: %w", err)
	}
	m.Seen = seen != 0
	if seenAt > 0 {
		m.SeenAt = time.Unix(seenAt, 0)
	}
	m.CreatedAt = time.Unix(created, 0)
	return &m, nil
}

func (s *Store) DeleteMovie(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM movies WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete movie: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSeen marks a movie watched or puts it back in the running.
//
// Marking as seen hands every voter their vote back: the vote rows stay (so we
// keep the record of who picked the winner) but they stop counting against the
// budget, because VotesUsed only looks at unseen movies.
//
// Un-marking has to be careful the other way: a voter may already have spent
// the returned vote elsewhere. Restoring their old vote would push them over
// the limit, so those votes are dropped instead.
func (s *Store) SetSeen(ctx context.Context, id int64, seen bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var seenAt any
	if seen {
		seenAt = time.Now().Unix()
	}
	res, err := tx.ExecContext(ctx, `UPDATE movies SET seen = ?, seen_at = ? WHERE id = ?`,
		boolToInt(seen), seenAt, id)
	if err != nil {
		return fmt.Errorf("set seen: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if !seen {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM votes
 WHERE movie_id = ?
   AND user_id IN (
       SELECT v.user_id FROM votes v
         JOIN movies m ON m.id = v.movie_id
        WHERE v.movie_id <> ? AND m.seen = 0
        GROUP BY v.user_id
       HAVING COUNT(*) >= ?)`, id, id, s.MaxVotes); err != nil {
			return fmt.Errorf("prune over-budget votes: %w", err)
		}
	}
	return tx.Commit()
}

/* ---------------------------------------------------------------- votes --- */

// VotesUsed counts a user's votes on movies that have not been seen yet.
func (s *Store) VotesUsed(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM votes v JOIN movies m ON m.id = v.movie_id
 WHERE v.user_id = ? AND m.seen = 0`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("votes used: %w", err)
	}
	return n, nil
}

func (s *Store) VotesLeft(ctx context.Context, userID int64) (int, error) {
	used, err := s.VotesUsed(ctx, userID)
	if err != nil {
		return 0, err
	}
	left := s.MaxVotes - used
	if left < 0 {
		left = 0
	}
	return left, nil
}

// Vote records one vote from a user for a movie. It enforces the whole rule
// set in a single transaction: one vote per movie, at most MaxVotes votes on
// unseen movies, and no voting for something already watched.
func (s *Store) Vote(ctx context.Context, userID, movieID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var seen int
	if err := tx.QueryRowContext(ctx, `SELECT seen FROM movies WHERE id = ?`, movieID).Scan(&seen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lookup movie: %w", err)
	}
	if seen != 0 {
		return ErrMovieSeen
	}

	var already int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM votes WHERE user_id = ? AND movie_id = ?`, userID, movieID).Scan(&already); err != nil {
		return fmt.Errorf("check vote: %w", err)
	}
	if already > 0 {
		return ErrAlreadyVoted
	}

	var used int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM votes v JOIN movies m ON m.id = v.movie_id
 WHERE v.user_id = ? AND m.seen = 0`, userID).Scan(&used); err != nil {
		return fmt.Errorf("count votes: %w", err)
	}
	if used >= s.MaxVotes {
		return ErrNoVotesLeft
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO votes (user_id, movie_id, created_at) VALUES (?, ?, ?)`,
		userID, movieID, time.Now().Unix()); err != nil {
		if isUniqueViolation(err) {
			return ErrAlreadyVoted
		}
		return fmt.Errorf("insert vote: %w", err)
	}
	return tx.Commit()
}

// Unvote takes a vote back. Votes on seen movies are historical record and
// cannot be withdrawn.
func (s *Store) Unvote(ctx context.Context, userID, movieID int64) error {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM votes
 WHERE user_id = ? AND movie_id = ?
   AND movie_id IN (SELECT id FROM movies WHERE seen = 0)`, userID, movieID)
	if err != nil {
		return fmt.Errorf("unvote: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

/* --------------------------------------------------------------- helpers --- */

func ci(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite reports constraint failures in the message; matching
	// on it keeps us free of a driver-specific error type import.
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}
