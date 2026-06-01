### JSON file

**vocative.json** contains about 1800 known names with vocative form in Serbia and Balkans.

# New: NPM Package
```cmd
npm install deklinacija
```
```js
import dekl from 'deklinacija'
dekl("Ognjen").vocative      //string or null
dekl("Ognjen").vocativeCyr   //string or null
dekl("Ognjen").sex           // "male", "female", "both" or null
```

# Deklinacija API

[deklinacija.com](https://deklinacija.com)

Free commercial use. There is no limit on usage or pricing, but please be respectful and don't abuse the API.

Slobodno korišćenje API-a za komercijalne svrhe. Nema ograničenja niti cene, ali vas molim da budete pošteni i da ne zloupotrebljavate API.

## Run the server yourself

The server lives in the [`server/`](server) folder. It loads `vocative.json`
into an embedded [BoltDB](https://github.com/etcd-io/bbolt) cache on startup and
serves fast, case-insensitive lookups. Cyrillic input is transliterated
automatically, and common diacritic-free spellings (`dj` → `đ`, `dz` → `dž`)
are matched as a fallback.

### Option A — Go

```bash
cd server
go run .
# → listening on :3009
```

Then open <http://localhost:3009> or query the API:

```bash
curl http://localhost:3009/api/Ognjen
```

### Option B — Docker

```bash
# from the repo root
docker build -t deklinacija-api -f server/Dockerfile .
docker run --rm -p 3009:3009 deklinacija-api
```

### Option C — Docker Compose

```bash
cd server
docker compose up --build
```

### Configuration

All configuration is via environment variables (all optional). Copy
`.env.example` to `.env` as a starting point.

| Variable       | Default                    | Description                                                       |
| -------------- | -------------------------- | ----------------------------------------------------------------- |
| `PORT`         | `3009`                     | Port the HTTP server listens on.                                  |
| `NAMES_PATH`   | `../vocative.json`         | Path to the names dataset (repo root, relative to `server/`).     |
| `DB_PATH`      | `names.db`                 | Path to the BoltDB cache (rebuilt from `NAMES_PATH` on startup).  |
| `POSTHOG_KEY`  | _(unset)_                  | PostHog project key. Analytics are **off** unless this is set.    |
| `POSTHOG_HOST` | `https://eu.i.posthog.com` | PostHog host, used only when `POSTHOG_KEY` is set.                |

Analytics are disabled by default so self-hosted instances never send data anywhere.

### Endpoints

| Endpoint            | Description                                          |
| ------------------- | --------------------------------------------------- |
| `GET /`             | Landing page with a live demo.                      |
| `GET /api/:name`    | Resolve a name (case-insensitive, Latin/Cyrillic).  |
| `GET /api/all`      | Returns the full `vocative.json` dataset.           |
| `GET /api/usage`    | Per-minute request counts for the last 60 minutes.  |
| `GET /api/requests` | Recent requests (last 60 minutes), newest first.    |

To add or fix names, edit `vocative.json` and restart the server — the cache is
rebuilt from it on every startup.

## Usage

GET `https://deklinacija.com/api/:name` (case-insensitive)

Supports Serbian Cyrillic input. For example, `/api/Огњен` is equivalent to `/api/Ognjen`.

Example request:
```http
GET /api/Ognjen HTTP/1.1
Host: localhost:3009
```

200 Success response:
```json
{
  "name": "Ognjen",
  "sex": "male",
  "vocative": "Ognjene",
  "vocative_cyr": "Огњене",
  "status": "Success"
}
```

404 Not Found response:
```json
{
  "name": "Ognjan",
  "sex": null,
  "vocative": null,
  "vocative_cyr": null,
  "status": "Not found"
}
```

## Response format

```typescript
{
  "name": string,
  "sex": "male" | "female" | "both" | null,
  "vocative": string | null,
  "vocative_cyr": string | null,
  "status": "Success" | "Not found"
}
```


