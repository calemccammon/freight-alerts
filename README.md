# 🚦 Freight Alerts

> Delay alerts for Finnish cargo rail. Users sign in with **GitHub**, register watch rules, and get notified when a train they care about runs late. A **Go** service over **Postgres**, polling the keyless [Digitraffic](https://www.digitraffic.fi/) open API — with the promise that it notifies you **once**, not on every poll.

Data flows: `cron → poll Digitraffic → evaluate rules → insert (deduped by the database) → webhook + feed`

---

## The Problem Worth Solving

A train that is 20 minutes late is still 20 minutes late on the next poll. And the next. Anything that re-reads upstream on a schedule and notifies on what it finds will notify you every single cycle unless something stops it.

The obvious fix — check whether we've already alerted, then insert — is wrong, and quietly so. Between the check and the insert, another run can do the same check and reach the same conclusion. Two notifications, from code that looks correct.

So the check isn't in the code:

```sql
UNIQUE (rule_id, train_number, departure_date)
```

Inserts use `ON CONFLICT DO NOTHING ... RETURNING`, and the service treats **the rows the database actually accepted** as the alerts that are new. An overlapping run loses the race atomically instead of double-notifying, and only genuinely new rows get a webhook.

Three decisions fall out of that key, each pinned by a test:

- **The delay is not in it.** Keying on the delay would re-fire every minute a train slipped further.
- **The departure date is.** The same train number runs daily; without the date, today's alert would suppress tomorrow's.
- **The rule is.** Two people watching the same train both get told.

---

## What This Project Demonstrates

| Concept | How It's Shown Here |
|---|---|
| **Transactional writes under concurrency** | Suppression is a `UNIQUE` constraint, not a read-then-write in application code |
| **Idempotent scheduled work** | A cycle can run twice, late, or overlapping without double-notifying |
| **Real authentication** | GitHub OAuth with CSRF state; no password is ever accepted, stored, or hashed |
| **Session hygiene** | Only the SHA-256 of a token is stored, so a database dump can't be replayed as live logins |
| **SSRF defence** | User-supplied webhook URLs are validated, re-checked at dial time, and never followed through redirects |
| **Schema migrations** | Embedded in the binary, transactional, advisory-locked against concurrent deploys |
| **Observability** | Every cycle writes a `poll_runs` row, so a service that quietly stopped alerting looks different from one with nothing to report |
| **Interfaces at the consumer** | The API and poller declare the narrow store interfaces they need, so their tests run with no database |
| **Operable failure modes** | A dead webhook fails one delivery, not the run; an upstream 503 is recorded, not swallowed |

---

## Architecture

Nothing runs between cycles. That is what makes it free to operate.

```
        ┌──────────────────────────────────────────────┐
        │  GitHub Actions cron  (*/15)                 │
        │    freightalerts poll   ── one pass, exits   │
        └───────────────┬──────────────────────────────┘
                        │  GraphQL (keyless)
                        ▼
              ┌───────────────────┐
              │ Digitraffic       │  live cargo trains
              └─────────┬─────────┘
                        │
                        ▼
        evaluate rules ──▶ INSERT ... ON CONFLICT DO NOTHING
                        │            │
                        │            └──▶ only new rows ──▶ webhook
                        ▼
              ┌───────────────────┐
              │ Postgres (Neon)   │◀──── freightalerts serve
              └───────────────────┘         GitHub OAuth,
                                            rule CRUD, alert feed
```

One binary, three subcommands: `migrate`, `poll`, `serve`. The poller is a single pass rather than a daemon with its own timer precisely so the schedule can live outside the process.

---

## Security

Three surfaces, each handled deliberately.

**Webhook URLs are attacker-controlled input.** A user-supplied URL that the server fetches is server-side request forgery: unguarded, this service becomes a proxy for reaching things only it can reach — cloud metadata at `169.254.169.254`, databases on the private network, admin panels on localhost. So:

- Only `http`/`https`; `file://` and friends are refused at rule creation, as a 400 the user sees rather than a failure buried in a log.
- Literal private addresses are rejected up front — but a *hostname* that resolves to one cannot be caught that way, so the dialer's `Control` hook re-checks the actual IP **after** DNS resolution, closing the rebinding gap.
- Redirects are never followed. A permitted public URL that `302`s to the metadata endpoint would otherwise walk straight past both checks.

**Sessions.** The cookie holds a 256-bit random token; the database holds only its SHA-256. `HttpOnly` keeps it away from page scripts, `SameSite=Lax` blocks cross-site posts while still allowing the OAuth redirect back. Logout deletes server-side — clearing the cookie alone would leave a working token with anyone who copied it.

**OAuth.** State is generated per attempt, stored in a short-lived cookie, compared in constant time, and cleared on use so it can't be replayed. Sign-in requests **no scopes**: the service learns who you are and gains access to nothing in your account.

---

## API

| Method | Path | |
|---|---|---|
| `GET` | `/healthz` | liveness |
| `GET` | `/auth/login` | redirect to GitHub |
| `GET` | `/auth/callback` | complete sign-in, set session |
| `POST` | `/auth/logout` | delete session server-side |
| `GET` | `/api/me` | current user |
| `POST` | `/api/tokens` | mint a device token for a native client (shown once) |
| `GET` | `/api/rules` | your rules |
| `POST` | `/api/rules` | create a rule |
| `DELETE` | `/api/rules/{id}` | delete a rule (yours only — another id is a 404, never a deletion) |
| `GET` | `/api/alerts` | your alert feed |

```jsonc
// POST /api/rules
{
  "kind": "operator",        // "train" | "operator" | "station"
  "target": "vr",            // train number, operator code, or station name
  "threshold_minutes": 15,   // fires at >= this many minutes late
  "webhook_url": "https://hooks.example.com/x"   // optional
}
```

A rule re-submitted identically returns the existing one rather than creating a second subscription that would double every future alert.

**Two transports, one credential.** Browsers carry the session in a cookie; native clients send the same token as `Authorization: Bearer …`. A device token is a session row like any other — hashed identically, expiring identically, revoked identically — so there is no second credential concept to secure.

---

## Getting Started

```bash
# Postgres for local development
docker run -d --name fa-pg -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=fa \
  -p 5432:5432 postgres:16-alpine
export DATABASE_URL='postgres://postgres:pw@127.0.0.1:5432/fa?sslmode=disable'

go build -o freightalerts ./cmd/freightalerts
./freightalerts migrate
./freightalerts poll     # one cycle against live Digitraffic
./freightalerts serve    # http://localhost:8080

go test ./...            # store tests skip without DATABASE_URL
```

Sign-in needs a GitHub OAuth app (`GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `GITHUB_REDIRECT_URL`). Without them the service still runs — the poller doesn't need OAuth — and `/auth/login` returns a clear 503 instead of a broken redirect. Set `INSECURE_COOKIES=1` for local http.

`ALLOWED_LOGINS` restricts who may sign in — a comma-separated list of GitHub logins, compared case-insensitively. **Unset means open**, so a fresh clone runs without configuration; a public deployment should set it. The check sits in the OAuth callback rather than in each handler, which is sufficient because every authenticated route requires a session and a session is only ever minted there. `serve` logs which mode it started in, so an unset variable is visible immediately rather than discovered later from an unexpected user row.

---

## Deployment

**The poller needs no host at all.** `.github/workflows/poll.yml` runs it on a cron with `DATABASE_URL` as a repository secret. Free, and nothing sleeps because nothing is running.

**Postgres:** any managed Postgres. Neon's free tier suits a workload that is idle between cycles.

**The API does need somewhere to run**, and this is the one place the plan bent. Cloudflare Workers was the original intent, but Workers run JavaScript and WASM — not a Go binary — so it would have meant splitting the service across two languages for a handful of endpoints. Options that keep it one Go binary:

| Host | Cost | Caveat |
|---|---|---|
| **Google Cloud Run** | Free tier, scales to zero | ~1s cold start; needs a GCP billing account attached |
| Fly.io / Railway | ~$5/mo | Always warm, no compromise |
| Render free tier | Free | Spins down; ~30s cold start on the demo |

The API is stateless and reads `PORT`. A `Dockerfile` is included — multi-stage,
static binary on distroless, running unprivileged, 21 MB:

```bash
docker build -t freight-alerts .
docker run -p 8080:8080 -e DATABASE_URL=... freight-alerts        # serve
docker run -e DATABASE_URL=... freight-alerts poll                # one cycle
```

**Nothing is deployed yet.** [`DEPLOY.md`](DEPLOY.md) is the runbook: database,
host, OAuth app, in that order — the OAuth callback needs a URL that only exists
once the API is running. The poller works after the database step alone; until
then the scheduled workflow detects the missing secret and skips cleanly rather
than failing every 15 minutes.

---

## Tests

**97 tests, none of which need a network or a credential.** The split is deliberate:

- **`rules`** — pure domain, zero infrastructure. A train late at one station and recovered at the next, a threshold exactly on the boundary, an estimate that hasn't happened yet.
- **`store`** — real Postgres, because the behaviour under test *is* the database. A fake would assert that the fake works. CI provides one as a service container; locally they skip without `DATABASE_URL`.
- **`api` / `poll`** — fakes against the narrow interfaces those packages declare, so handler and cycle logic test without either.

Verified end to end against live data before shipping: 23 real cargo trains, **10 alerts on the first poll, 0 on the second**.

---

## What This Doesn't Do

- **Rail only.** Digitraffic also publishes marine AIS and port calls; the corridor half of [`flutter-freight-corridor`](https://github.com/calemccammon/flutter-freight-corridor) is not here yet.
- **Webhooks aren't retried.** A failed delivery is recorded against the alert and the alert stays in the feed, but there is no backoff queue.
- **No rate limiting.** Fine for a single-tenant demo; the first thing to add for real users.
- **Cadence floors at the cron interval.** A 15-minute schedule means up to 15 minutes of notification latency — the deliberate cost of having no always-on host.

---

## Project Comparison

[`flutter-freight-corridor`](https://github.com/calemccammon/flutter-freight-corridor) reads the same Digitraffic APIs and renders them — but it is read-only, and its watchlist lives in device-local `shared_preferences` that evaporates on reinstall.

This is the other half: the write path. Accounts, persistence, transactional inserts under concurrency, background work. It's what turns that watchlist from a device setting into something with an owner, a server, and a history.

---

## Attribution & Data Licence

Train data comes from **[Fintraffic Digitraffic](https://www.digitraffic.fi/en/)** and is licensed under **[Creative Commons Attribution 4.0 International (CC BY 4.0)](https://creativecommons.org/licenses/by/4.0/)**.

CC BY 4.0 permits commercial use and redistribution, and requires attribution in return — credit to the source, a link to the original, an indication of the licence, and a note where the material has been modified. This service **modifies** the data: it derives a current delay from the most recently reached timetable stop and evaluates it against user-defined thresholds. Alerts are therefore a derived work, not raw Digitraffic output.

That attribution travels with the data rather than living only here: `GET /api/alerts` returns an `attribution` field alongside the alerts, so any client consuming this API carries the credit too.

No API key is required and none is used. Requests identify themselves with the `Digitraffic-User` header, as Fintraffic's instructions ask, and `Accept-Encoding: gzip` is left to Go's transport (Digitraffic answers `406` without it).

---

## License

MIT — the code. The train data is Fintraffic's, under CC BY 4.0, as above.
