# CineVote 🎬

[![CI](https://github.com/o5ten/cinevote/actions/workflows/ci.yml/badge.svg)](https://github.com/o5ten/cinevote/actions/workflows/ci.yml)

A small movie voting board for planned movie nights. Everyone adds suggestions,
everyone gets five votes to spread around, and the film the most people backed
is the one you watch. Once it has been watched the admin marks it as seen — and
everyone who voted for it gets that vote back for next time.

Go, SQLite and server-rendered HTML. One static binary, no frontend build step,
no runtime dependencies. The interface is in Swedish; everything else is here.

## Demo mode — zero configuration

Just want to see it? Start it with `-demo`. No API key, no admin password, no
database to set up:

From a clone:

```bash
git clone https://github.com/o5ten/cinevote.git
cd cinevote
go run ./cmd/cinevote -demo        # or: make demo
docker compose --profile demo up   # or: make compose-demo
```

Or straight from the published image, nothing to clone (available once CI has
built a push to `main`, and the package has been made public in GHCR):

```bash
docker run --rm -p 8080:8080 ghcr.io/o5ten/cinevote:latest -demo
```

Open <http://localhost:8080> and click one of the demo accounts on the login
page — it fills the form in for you. The password is `demo1234` for all of them.

Demo mode creates five accounts (`admin`, Anna, Björn, Cissi, David), ten films
with real posters, ratings, directors and cast, votes already cast, and two
films already watched — so the leaderboard, the vote budget, the filters and the
"similar films" scoring all have something to show immediately. Three of the
films deliberately share a director so the similarity ranking has something to
find.

Demo mode always uses a throwaway database that is recreated on every start,
whatever `CINEVOTE_DB` says, so it can never touch a real one. It only turns on
via `-demo` or `CINEVOTE_DEMO=true`, never by itself. Don't use it for an actual
movie night: everyone shares one obvious password.

## Running it for real

### With Docker Compose

```bash
git clone https://github.com/o5ten/cinevote.git && cd cinevote
cp .env.example .env      # fill in OMDB_API_KEY
docker compose up --build -d
docker compose logs -f    # the admin password is in here if you didn't set one
```

### Locally

Needs Go 1.22 or newer — nothing else, the database driver is pure Go.

```bash
export OMDB_API_KEY=...   # optional, but posters stay blank without it
make run                  # or: go run ./cmd/cinevote
```

The database is a single SQLite file (`data/cinevote.db` locally,
`/data/cinevote.db` in the container). Back it up by copying the file.

### First login

Set `CINEVOTE_ADMIN_PASSWORD` before the first start, or let the app generate
one and print it **once**:

```
level=WARN msg="generated admin password — save it now, it is not shown again" username=admin password=...
```

Everyone else signs up at `/register`. Set `CINEVOTE_REGISTRATION_CODE` if the
site is reachable from the internet — then a shared invite code is required.

## Movie data and posters

Posters, ratings, runtime, genre, director, cast and plot come from **OMDb**,
the free API for IMDb data.

> **Get a free API key here: <https://www.omdbapi.com/apikey.aspx>**
> Pick "FREE (1,000 daily limit)", enter your email, and put the key you get
> into `.env` as `OMDB_API_KEY`.

The app shows that same link in the UI while no key is configured — in the
suggestion form, on the admin page and in the demo banner — and prints it in the
log at startup.

How lookups work:

* Type a title and pause; the app searches OMDb automatically (600 ms after the
  last keystroke) and drops the hits in a list attached to the field.
* Walk the list with ↑/↓ (it wraps, and Home/End jump to the ends), take the
  highlighted one with Enter, or click it. ↓ on a closed list reopens it, Escape
  and Tab close it, and a click outside does too.
* Search hits are enriched with rating, genre and director before being shown,
  and those lookups are cached, so repeated typing does not burn through the
  daily quota.
* Hits are ordered best first, where "best" weighs the rating against how many
  people gave it: an exact title match wins outright, then films with a lot of
  ratings, then rating. Without that, searching "Inception" offers you an
  obscure short rated 8.9 by 187 people ahead of the 8.8 one rated by three
  million.
* Submit without picking anything and the server resolves the title itself.
* The server always re-resolves the film from its IMDb id before saving, so the
  browser cannot invent metadata and the API key never leaves the server.
* Without a key everything still works — paste a poster URL yourself.

### Watching the API allowance

The free OMDb tier allows 1,000 requests a day, and the API reports nothing
about what's left — no header, no field. So CineVote counts what it sends: the
footer shows how many calls remain, and the admin page has a meter with the
detail. The count starts fresh whenever the app starts, so it is a floor rather
than gospel if the key is shared with something else.

One search costs one request plus one per result shown, because the search
response carries no rating and the results have to be enriched before they can
be ranked. That's the expensive part, so whole result sets are cached: repeating
a search, or typing towards a title you already looked up, is free. If the
allowance does run out, the search box says so plainly and you can still add
films by hand.

### Similar films

Every card has a **Liknande** link. That page has two halves:

1. **Films already on the board** that resemble it, scored on shared director,
   genres and cast, plus era and rating, with the reasons listed under each
   card. Works offline, no key needed.
2. **Suggestions from outside the list**, for discovering films nobody has
   proposed yet. OMDb has no recommendation endpoint, so this half needs a free
   TMDB key: <https://www.themoviedb.org/settings/api>, then
   `TMDB_API_KEY=...`. Metadata and posters keep coming from OMDb; TMDB is only
   asked "what is like this?". Each suggestion has a one-click **Föreslå den
   här** button. Without the key, the page explains how to enable it instead of
   pretending there is nothing to show.

TMDB can also replace OMDb entirely as the metadata backend
(`CINEVOTE_POSTER_SOURCE=tmdb`), which handles non-English titles better.

## Filtering and sorting

The board has a filter bar, and every filter is a URL parameter, so a filtered
view is a link you can paste in the group chat:

| Parameter | Values |
| --- | --- |
| `q` | free text across title, director, cast, genre, plot and who suggested it — every word has to match |
| `genre` | one genre, from the films actually on the board |
| `director` | one director, likewise |
| `min_rating` | `6`, `7`, `8`, `8.5` … films without a rating are excluded |
| `sort` | `votes` (default), `rating`, `year`, `new`, `title` |
| `show` | `open` (default), `all`, `seen` |
| `view` | `cards` (default) or `list` |

Equal vote counts are broken by rating, so the better-reviewed film is listed
first — the same preference applies wherever films are ranked. While a filter is
active the podium is hidden: those top-three ranks describe the whole board, and
showing them beside a filtered list would be misleading.

### Two layouts

The toggle in the filter bar switches between **Kort** (poster cards, the
default) and **Lista** — one table with rating, runtime, genre, director, votes
and voters side by side, for when the board has grown past comfortable
scrolling. You can vote straight from a row, and the top three keep their
medals. The choice is remembered, and voting returns you to the same view, the
same filters and the row you were on.

## Configuration

Everything is environment variables. The only flags are `-demo` and `-version`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `CINEVOTE_ADDR` | `:8080` | Listen address |
| `CINEVOTE_DB` | `data/cinevote.db` | SQLite file (ignored in demo mode) |
| `CINEVOTE_SITE_NAME` | `CineVote` | Name in the header |
| `CINEVOTE_MAX_VOTES` | `5` | Votes per user |
| `CINEVOTE_ADMIN_USERNAME` | `admin` | The single admin account |
| `CINEVOTE_ADMIN_PASSWORD` | *(generated)* | Set at start; updates the password if changed |
| `CINEVOTE_REGISTRATION_CODE` | *(empty)* | Require an invite code to sign up |
| `CINEVOTE_SESSION_DAYS` | `30` | How long a login lasts |
| `CINEVOTE_SECURE_COOKIES` | `false` | Set `true` behind HTTPS |
| `CINEVOTE_DEMO` | `false` | Demo mode, same as `-demo` |
| `OMDB_API_KEY` | *(empty)* | Enables IMDb lookups |
| `CINEVOTE_POSTER_SOURCE` | *(auto)* | `imdb`, `tmdb` or `none` |
| `TMDB_API_KEY` | *(empty)* | Enables "similar films" from outside the list |

## How the voting works

* Every user has five votes and may place **one** vote per film.
* Votes can be moved right up until movie night — take a vote back and put it
  somewhere else.
* A vote on a film that has been **seen** does not count against the budget, but
  it stays in the record so you can see who picked it.
* If the admin puts a seen film back into the running, any votes whose owners
  have already re-spent them are dropped — otherwise someone would have six
  votes in play.
* You can withdraw your own suggestion as long as nobody has voted for it.
  After that it is the admin's call.

## Development

```bash
make demo        # seeded demo on :8080, needs no configuration
make test        # go test -race ./...
make lint        # gofmt + go vet, same as CI
make build       # static binary in dist/
make docker      # build the container image
```

Tests cover the voting rules at the database level (`internal/store`), the OMDb
client against a fake API server including rating-ranked search and caching
(`internal/poster`), filtering, sorting and similarity scoring
(`internal/browse`), the demo seed data (`internal/demo`), and the whole HTTP
flow — login, CSRF, voting, admin, filters, the similar page and the demo logins
(`internal/web`). No test touches the network.

### Layout

```
cmd/cinevote        main, configuration, graceful shutdown
internal/config     environment variables
internal/auth       password hashing, tokens, validation
internal/store      SQLite: users, sessions, movies, votes
internal/browse     filtering, sorting and similarity scoring
internal/demo       seed data for demo mode (accounts, films, votes)
internal/poster     OMDb/TMDB lookups behind one interface
internal/web        routing, sessions, HTML templates, CSS/JS (embedded)
```

The server is a single static binary: templates, CSS, JS and favicon are built
in with `go:embed`, and the SQLite driver is pure Go (`modernc.org/sqlite`), so
`CGO_ENABLED=0` is enough and the image needs nothing but `ca-certificates`.

Asset URLs carry a hash of the embedded files (`/static/app.js?v=…`), so they
can be cached hard and a deploy still can't leave anyone on the old CSS or JS.
Opening a database written by an older version adds any columns it is missing,
so upgrading in place needs no manual migration.

### Security

Passwords are hashed with bcrypt. Sessions are random 256-bit tokens in
HttpOnly cookies with server-side expiry. Every form carries a session-bound
CSRF token. Login and registration are rate limited per IP. Responses set a CSP
that only allows the app's own CSS and JS — which is why there are no inline
styles or scripts in the templates, and no external fonts.

## CI/CD

`.github/workflows/ci.yml` runs on every push and pull request:

1. **test** — `gofmt` check, `go mod tidy` check, `go vet`, `go test -race` with coverage.
2. **build** — cross-compiles binaries for linux/amd64, linux/arm64 and darwin/arm64.
3. **image** — builds for amd64 + arm64, pushes to `ghcr.io/o5ten/cinevote`
   (not on pull requests) and smoke-tests demo mode in a running container.

Image tags:

| Event | Tags |
| --- | --- |
| push to `main` | `latest`, `main`, `sha-<short sha>` |
| tag `v1.2.3` | `1.2.3`, `1.2`, `sha-<short sha>` |
| pull request | `pr-<number>`, `sha-<short sha>` — built, not pushed |

So `:latest` always follows `main`, never a version tag. To make releases move
`latest` instead, change `enable={{is_default_branch}}` to
`enable={{is_default_branch}} || startsWith(github.ref, 'refs/tags/v')` in
`.github/workflows/ci.yml`.

The first push to `main` creates the GHCR package. It is private by default —
make it public under **Packages → cinevote → Package settings** if anyone should
be able to `docker pull` it.

## License

MIT.
