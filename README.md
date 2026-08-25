# CineVote 🎬

Ett litet filmröstningssystem för planerade filmkvällar. Alla lägger in förslag,
alla har fem röster att fördela, och den film som flest personer röstat på är
den som visas. När filmen är sedd markerar admin den — och alla som röstade på
den får tillbaka sin röst till nästa gång.

Byggt i Go (inga externa beroenden i frontend), paketerat med Docker och byggt
i GitHub Actions.

## Kravlistan

| Krav | Hur det är löst |
| --- | --- |
| Unika användare | Konto med användarnamn + lösenord (bcrypt). Användarnamn är unika oavsett versaler. |
| Admin (en enda) | Kontot som anges i `CINEVOTE_ADMIN_USERNAME` (default `admin`). Databasen tillåter bara en admin — övriga degraderas automatiskt. |
| Alla kan lägga in filmförslag | Formuläret på startsidan. Söker fram poster och betyg automatiskt. |
| Fem röster per användare att fördela | Konfigurerbart med `CINEVOTE_MAX_VOTES`, default 5. Räknas och visas som prickar i headern. |
| En röst per film | Databasnyckel `(user_id, movie_id)` — dubbelröstning är omöjlig, inte bara otillåten i UI:t. |
| Admin kan markera filmer som sedda | Knapp på varje kort och i admin-tabellen. |
| Röster på sedda filmer återlämnas | Rösterna ligger kvar som historik men räknas inte mot budgeten. Se `SetSeen` i `internal/store/store.go`. |
| De 3 mest röstade visas prominent | Egen "Toppen"-sektion med medaljer, och ledaren annonseras i hero-rutan som nästa filmkväll. |
| Filmposters i UI:t | Hämtas från OMDb (IMDb-data) vid förslag, eller klistras in manuellt. |

## Demoläge — noll konfiguration

Vill du bara se hur det ser ut? Starta med `-demo`. Ingen API-nyckel, inget
admin-lösenord, ingen databas att sätta upp:

```bash
go run ./cmd/cinevote -demo        # eller: make demo
docker compose --profile demo up   # eller: make compose-demo
docker run --rm -p 8080:8080 -e CINEVOTE_DB= ghcr.io/mikaelo/cinevote -demo
```

Öppna <http://localhost:8080> och klicka på ett av demokontona på
inloggningssidan — de fylls i automatiskt. Lösenordet är `demo1234` för alla.

Demoläget skapar fem konton (`admin`, Anna, Björn, Cissi, David), tio filmer med
riktiga posters och betyg, utlagda röster och två filmer som redan är sedda — så
topplistan, röstbudgeten och "sedd"-funktionen syns direkt. Data ligger i en
tillfällig databas som nollställs vid varje omstart, och en gul ruta i UI:t
påminner om att inget är på riktigt.

Demoläget slås bara på med `-demo` eller `CINEVOTE_DEMO=true` — aldrig av sig
självt. Använd det inte för en riktig filmkväll: alla delar ett känt lösenord.

## Kom igång

### Med Docker Compose

```bash
cp .env.example .env      # fyll i OMDB_API_KEY
docker compose up --build -d
docker compose logs -f     # här står admin-lösenordet om du inte satt något
```

Öppna <http://localhost:8080>.

### Lokalt

```bash
export OMDB_API_KEY=...        # frivilligt, men posters blir tomma utan
make run                       # eller: go run ./cmd/cinevote
```

Databasen är en SQLite-fil (`data/cinevote.db` lokalt, `/data/cinevote.db` i
containern). Ta backup genom att kopiera filen.

### Första inloggningen

Sätt `CINEVOTE_ADMIN_PASSWORD` innan första starten, eller låt appen generera
ett lösenord som skrivs ut **en gång** i loggen:

```
level=WARN msg="generated admin password — save it now, it is not shown again" username=admin password=...
```

Övriga skapar egna konton på `/register`. Sätt `CINEVOTE_REGISTRATION_CODE` om
sidan är öppen mot internet — då krävs en delad inbjudningskod.

## Filmdata och posters

Poster, betyg, speltid, genre och handling hämtas från **OMDb** — det fria
API:et för IMDb-data.

> **Skaffa en gratis API-nyckel här: <https://www.omdbapi.com/apikey.aspx>**
> Välj "FREE (1,000 daily limit)", fyll i din mejl och klistra in nyckeln du får
> som `OMDB_API_KEY` i `.env`.

Appen visar samma länk i UI:t så länge ingen nyckel är satt — i förslagsformuläret,
på adminsidan och i demorutan — och skriver ut den i loggen vid start.

* I formuläret: skriv en titel, tryck **Sök på IMDb**, välj rätt film.
* Skickar du in utan att söka gör servern en bästa-träff-sökning på titeln.
* Servern slår alltid upp filmen på nytt via `imdb_id` innan den sparas, så
  webbläsaren kan inte hitta på metadata. API-nyckeln lämnar aldrig servern.
* Utan nyckel fungerar allt utom automatiken — klistra in en posterlänk själv.

TMDB finns som alternativ backend (`CINEVOTE_POSTER_SOURCE=tmdb` +
`TMDB_API_KEY`), t.ex. om ni röstar på mycket icke-engelska titlar.

## Konfiguration

Allt styrs med miljövariabler:

| Variabel | Default | Betydelse |
| --- | --- | --- |
| `CINEVOTE_ADDR` | `:8080` | Lyssnaradress |
| `CINEVOTE_DB` | `data/cinevote.db` | Sökväg till SQLite-filen |
| `CINEVOTE_SITE_NAME` | `CineVote` | Namn i headern |
| `CINEVOTE_MAX_VOTES` | `5` | Röster per användare |
| `CINEVOTE_ADMIN_USERNAME` | `admin` | Det enda admin-kontot |
| `CINEVOTE_ADMIN_PASSWORD` | *(genereras)* | Sätts vid start; uppdaterar lösenordet om det ändras |
| `CINEVOTE_REGISTRATION_CODE` | *(tom)* | Kräv inbjudningskod vid registrering |
| `CINEVOTE_SESSION_DAYS` | `30` | Hur länge en inloggning gäller |
| `CINEVOTE_SECURE_COOKIES` | `false` | Sätt `true` bakom HTTPS |
| `CINEVOTE_DEMO` | `false` | Demoläge, samma som flaggan `-demo` |
| `OMDB_API_KEY` | *(tom)* | Aktiverar IMDb-uppslag |
| `CINEVOTE_POSTER_SOURCE` | *(auto)* | `imdb`, `tmdb` eller `none` |
| `TMDB_API_KEY` | *(tom)* | För `CINEVOTE_POSTER_SOURCE=tmdb` |

## Så funkar röstningen

* Varje användare har fem röster och kan lägga **en** röst per film.
* Rösterna kan flyttas fram till filmkvällen — ta tillbaka en röst och lägg den
  någon annanstans.
* En röst på en **sedd** film räknas inte mot budgeten, men står kvar i
  historiken så man ser vilka som valde filmen.
* Öppnar admin en sedd film igen tas de röster bort som ägarna redan hunnit
  återanvända — annars skulle någon kunna ha sex röster ute samtidigt.
* Egna förslag kan tas bort så länge ingen röstat på dem. Därefter är det
  admins bord.

## Utveckling

Flaggor: `-demo` (demoläge) och `-version`. Allt annat är miljövariabler.

```bash
make demo        # seedad demo på :8080, kräver ingen konfiguration
make test        # go test -race ./...
make lint        # gofmt + go vet, samma som i CI
make build       # statisk binär i dist/
make docker      # bygger containerimagen
```

Testerna täcker röstreglerna på databasnivå (`internal/store`), OMDb-klienten
mot en fejkad API-server (`internal/poster`), seeddatan för demoläget
(`internal/demo`) och hela HTTP-flödet inklusive inloggning, CSRF, röstning,
admin och demoinloggningarna (`internal/web`). Inget test rör nätet.

### Struktur

```
cmd/cinevote        main, konfiguration, graceful shutdown
internal/config     miljövariabler
internal/auth       lösenordshashning, tokens, validering
internal/store      SQLite: användare, sessioner, filmer, röster
internal/demo       seeddata för demoläget (konton, filmer, röster)
internal/poster     OMDb/TMDB-uppslag bakom ett gemensamt gränssnitt
internal/web        routing, sessioner, HTML-mallar, CSS/JS (embeddade)
```

Servern är en enda statisk binär: mallar, CSS, JS och favicon är inbyggda med
`go:embed`, och SQLite-drivaren är ren Go (`modernc.org/sqlite`), så
`CGO_ENABLED=0` räcker och imagen behöver inget mer än `ca-certificates`.

### Säkerhet

Lösenord hashas med bcrypt. Sessioner är slumpade 256-bitars tokens i
HttpOnly-cookies med serversidig utgång. Alla formulär skickar en
sessionsbunden CSRF-token. Inloggning och registrering är rate-limitade per
IP. Svaren sätter en CSP som bara tillåter egen CSS/JS — därför finns inga
inline-styles eller inline-script i mallarna, och inga externa fonter.

## CI/CD

`.github/workflows/ci.yml` kör vid varje push och PR:

1. **test** — `gofmt`-koll, `go mod tidy`-koll, `go vet`, `go test -race` med täckning.
2. **build** — korskompilerar binärer för linux/amd64, linux/arm64 och darwin/arm64.
3. **image** — bygger imagen för amd64 + arm64, pushar till
   `ghcr.io/<repo>` (inte på PR:er) och rök-testar `/healthz` i en körande container.

Taggar (`v1.2.3`) blir imagetaggar `1.2.3`, `1.2` och `latest`.

## Licens

MIT.
