# Freight Alerts

Delay alerts for Finnish cargo rail. Users sign in with GitHub, register watch rules, and get notified when a matching train runs late. It is a Go service over Postgres that polls the keyless [Digitraffic](https://www.digitraffic.fi/en/) open rail API on a schedule and sends one alert per rule, train and departure date, however many times the poller runs.

```
cron -> poll Digitraffic -> evaluate rules -> insert (deduplicated by the database) -> webhook + feed
```

What is worth a look:

- **Duplicate suppression is a database constraint.** Alerts are inserted with `INSERT ... ON CONFLICT DO NOTHING` against a `UNIQUE (rule_id, train_number, departure_date)` key, so two overlapping polls cannot both send the same alert.
- **Webhook URLs are guarded against SSRF.** Private addresses are rejected when a rule is created, the IP is checked again at connect time after DNS resolution, and redirects are not followed.
- **GitHub OAuth asks for no scopes**, and the database stores only a SHA-256 hash of each session token.
- **The store tests run against a real Postgres**, because what they test is the schema.

There is no live instance at present. The service ran on Google Cloud Run with a scheduled GitHub Actions poller from 2026-08-16 to 2026-10-15; see [Deployment](#deployment).

## Running it

You need Go 1.25 and a Postgres database.

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

`poll` and `serve` apply pending migrations before they start, so `migrate` on its own is only for preparing a database ahead of time.

| Variable | Used by | Meaning |
|---|---|---|
| `DATABASE_URL` | all | Postgres connection string. Required. |
| `PORT` | `serve` | Listen port. Defaults to 8080. |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `GITHUB_REDIRECT_URL` | `serve` | GitHub OAuth app credentials and callback URL. |
| `ALLOWED_LOGINS` | `serve` | Comma-separated GitHub logins allowed to sign in. Unset means any GitHub account. |
| `CORS_ORIGINS` | `serve` | Comma-separated browser origins allowed to call the API cross-origin. Unset means none. |
| `INSECURE_COOKIES` | `serve` | Set to `1` to drop the cookie `Secure` flag for local http. |

Without the OAuth variables the service still starts: the poller does not need them, and `/auth/login` returns a 503 with a JSON error.

The `Dockerfile` is a multi-stage build that produces a static binary on a distroless image running as a non-root user.

```bash
docker build -t freight-alerts .
docker run -p 8080:8080 -e DATABASE_URL=... freight-alerts        # serve
docker run -e DATABASE_URL=... freight-alerts poll                # one cycle
```

## Architecture

The poller is a single pass that exits. The schedule lives outside the process, in `.github/workflows/poll.yml` (every 15 minutes), so nothing has to stay running between cycles. One cycle:

1. Records a row in `poll_runs`.
2. Loads the active rules. If there are none it stops without calling Digitraffic.
3. Fetches the cargo trains currently running from the Digitraffic GraphQL endpoint.
4. Evaluates every rule against every train. A train's delay is its delay at the most recent stop it has actually reached; estimates for future stops are ignored, and so is an earlier delay the train has since made up.
5. Inserts the candidate alerts and keeps the rows the database accepted.
6. Posts a webhook for each accepted row whose rule has a webhook URL, and records the delivery outcome on the alert.
7. Updates the `poll_runs` row with the counts and any error.

| Package | Role |
|---|---|
| `cmd/freightalerts` | Subcommands, environment configuration, HTTP server lifecycle |
| `internal/rules` | Rule validation, matching and delay evaluation. No I/O. |
| `internal/digitraffic` | GraphQL client for running cargo trains |
| `internal/poll` | One poll cycle, written against narrow `Fetcher`, `Store` and `Notifier` interfaces |
| `internal/store` | Postgres queries (pgx) and the migration runner |
| `internal/webhook` | Webhook URL validation and delivery |
| `internal/auth` | GitHub OAuth code exchange, session tokens and cookies |
| `internal/api` | HTTP handlers, sign-in allowlist, CORS |
| `migrations` | SQL embedded in the binary |

### Data model

| Table | Contents | Constraints of note |
|---|---|---|
| `users` | GitHub id and login | `github_id` unique. No password column. |
| `sessions` | SHA-256 of a session token, user, expiry | Primary key is the hash |
| `watch_rules` | kind, target, threshold, optional webhook URL | `UNIQUE (user_id, kind, target, threshold_minutes)`, threshold between 1 and 1440 |
| `alerts` | rule, train number, departure date, operator, delay, station, webhook status | `UNIQUE (rule_id, train_number, departure_date)` |
| `poll_runs` | start and finish time, counts, error text | One row per cycle |

### API

| Method | Path | |
|---|---|---|
| `GET` | `/health` | liveness, no session required |
| `GET` | `/auth/login` | redirect to GitHub |
| `GET` | `/auth/callback` | complete sign-in, set the session cookie |
| `POST` | `/auth/logout` | delete the session (cookie or bearer token) and clear the cookie |
| `GET` | `/api/me` | current user |
| `POST` | `/api/tokens` | create a bearer token for a non-browser client; the plaintext is returned once |
| `GET` | `/api/rules` | the caller's rules |
| `POST` | `/api/rules` | create a rule |
| `DELETE` | `/api/rules/{id}` | delete one of the caller's rules; another user's id returns 404 |
| `GET` | `/api/alerts` | the caller's alerts, newest first; `limit` defaults to 100, maximum 500 |

```jsonc
// POST /api/rules
{
  "kind": "operator",        // "train" | "operator" | "station"
  "target": "vr",            // train number, operator code, or station name
  "threshold_minutes": 15,   // fires at >= this many minutes late, 1 to 1440
  "webhook_url": "https://hooks.example.com/x"   // optional
}
```

Submitting a rule with the same kind, target and threshold as an existing one returns the existing rule's id and replaces its webhook URL instead of creating a second subscription. A `train` rule matches that train number on every departure date.

Every `/api/` route requires a session: browsers send the `fa_session` cookie, other clients send `Authorization: Bearer <token>`. A token from `/api/tokens` is a `sessions` row like a browser session, hashed the same way and expiring after the same 30 days. `GET /api/alerts` includes the data licence notice in an `attribution` field.

The Flutter app [`flutter-freight-corridor`](https://github.com/calemccammon/flutter-freight-corridor) is a client of this API; it registers its watchlist as rules using a bearer token.

## Design decisions

### Duplicate suppression is a database constraint

A train that is 20 minutes late is still late on the next poll, so a poller that notifies on whatever it finds would notify every cycle. Checking for an existing alert and then inserting is not safe either: two overlapping runs can both pass the check before either inserts. So the check is a constraint on `alerts`:

```sql
UNIQUE (rule_id, train_number, departure_date)
```

`InsertAlerts` uses `INSERT ... ON CONFLICT DO NOTHING RETURNING id` and treats the returned rows as the new alerts. Only those get a webhook; a run that overlaps another loses the conflict and sends nothing.

Each choice of key column has a test in `internal/store/store_test.go`:

- The delay is not in the key, so a train slipping from 20 to 21 minutes late does not alert again.
- The departure date is in the key, because the same train number runs daily and the first day's alert would otherwise suppress every later day's.
- The rule id is in the key, so two users watching the same train are both notified.

### Webhook URLs are untrusted input

A URL that a user supplies and the server fetches is a server-side request forgery (SSRF) risk. `internal/webhook` checks it three ways:

- When a rule is created, the URL must be `http` or `https` with a host, and a literal loopback, private, link-local, multicast or unspecified address is rejected with a 400.
- A hostname that resolves to such an address cannot be caught at creation. The HTTP client's dialer has a `Control` hook that checks the IP actually being connected to, after DNS resolution, and refuses non-public addresses. This also covers a DNS record that changes after the rule was created.
- Redirects are not followed, so a public URL cannot bounce the request to an internal address.

The transport sets no proxy, so the dial-time check sees the real destination.

### Sessions and OAuth

- A session token is 32 random bytes. The database stores only its SHA-256, so a copy of the `sessions` table holds no usable tokens. The hash is unsalted and fast because the input is already high-entropy.
- The session cookie is `HttpOnly`, `SameSite=Lax`, and `Secure` unless `INSECURE_COOKIES=1`.
- Logout deletes the session row as well as clearing the cookie, and does the same for a bearer token sent to `POST /auth/logout`.
- The OAuth `state` value is generated per sign-in attempt, kept in a 10-minute `HttpOnly` cookie, compared in constant time on callback, and cleared whether or not it matches.
- The authorization request asks for no scopes. The service reads the user's id and login and gets no access to the account.
- The OAuth flow is written directly against `net/http` in `internal/auth/github.go`; for one provider it is two requests.

### Access policy

`ALLOWED_LOGINS` is checked in the OAuth callback before the user row is created. The callback is the only place a browser session is created, so that one check covers the whole API. Logins are compared case-insensitively. Unset or empty leaves sign-in open so a fresh clone works without configuration, and `serve` logs a warning at startup when that is the case.

`CORS_ORIGINS` defaults the other way: unset allows no cross-origin access. An allowed origin is echoed in `Access-Control-Allow-Origin` rather than a wildcard, and `Access-Control-Allow-Credentials` is never sent, so browsers do not attach the session cookie cross-origin and a cross-origin client has to use a bearer token.

### Migrations

Migrations are SQL files embedded in the binary and applied by a small hand-written runner in `internal/store/migrate.go`. Each runs in its own transaction, and a Postgres advisory lock serialises instances that start at the same time. They are forward-only. goose and golang-migrate were skipped because they pull in drivers for databases this service does not use; the only direct dependency in `go.mod` is `pgx`.

### Poll bookkeeping

Each cycle writes a `poll_runs` row with its counts and any error, which tells a poller that has stopped apart from one that found nothing. A failed webhook is logged, recorded on the alert as `webhook_status`, and the cycle carries on with the remaining alerts.

## Deployment

[`DEPLOY.md`](DEPLOY.md) is the runbook: the database, the API host and the GitHub OAuth app, in the order they have to be set up.

The poller needs no host. `.github/workflows/poll.yml` runs `freightalerts poll` on a `*/15` cron with `DATABASE_URL` as a repository secret; if the secret is missing, the workflow skips the poll and succeeds.

The API is a stateless container that reads `PORT`. It was deployed to Google Cloud Run in `us-east5`, scaling to zero. (Cloudflare Workers was considered first and ruled out because it does not run a Go binary.)

That deployment is no longer running. The scheduled workflow ran from 2026-08-16 to 2026-10-15, 694 runs of which 688 succeeded, until GitHub disabled the schedule after 60 days without repository activity; re-enabling it is a manual step on the Actions tab. The Cloud Run URL no longer answers `/health`.

Two lessons from it:

- The liveness route is `/health` because on Cloud Run, `/healthz` is answered by Google's front end with a 404 and never reaches the container.
- Put the region near Postgres, not near Digitraffic. The API never calls Digitraffic, and both the API and the Actions runners talk to the database.

## Tests

No test calls an external service.

- `internal/store` runs against a real Postgres, because the behaviour under test is the schema: the `ON CONFLICT` suppression, the `CHECK` constraints and the cascades. These tests skip when `DATABASE_URL` is unset.
- `internal/api` and `internal/poll` each declare the dependencies they need (store, OAuth provider, fetcher, notifier) as small interfaces, so their tests run against in-memory fakes with no database.
- `internal/auth`, `internal/webhook` and `internal/digitraffic` run against `httptest` servers standing in for GitHub, webhook receivers and Digitraffic.
- `internal/rules` tests pure functions.

CI (`.github/workflows/ci.yml`) starts a Postgres 16 service container, checks `gofmt`, runs `go vet` and `go test -race ./...`, builds the binary and runs `migrate` against the database.

The suppression tests insert the same alert twice in sequence; no test runs two polls concurrently.

## Limitations

- Rail only. Digitraffic also publishes marine AIS and port calls, which this service does not read.
- Webhooks are not retried. A failed delivery is recorded on the alert and the alert stays in the feed, but if the process dies between the insert and the webhook, that alert is never delivered.
- No rate limiting on the API or on rule creation.
- Notification latency depends on the schedule, and GitHub's cron is best-effort: while deployed, the `*/15` schedule fired about once every two hours (see [Deployment](#deployment)).
- A bearer token can only be revoked by a caller that holds it. There is no endpoint to list or revoke a user's other sessions; revoking a lost token means deleting its row, as described in `DEPLOY.md`.
- The poller purges expired sessions, so they pile up while it is not running. An expired session is rejected either way.
- The webhook address check covers loopback, RFC 1918, link-local, multicast, unspecified and IPv6 unique-local addresses. Other reserved ranges, such as 100.64.0.0/10, are not blocked.
- Rules cannot be paused through the API. The `active` column exists, but no endpoint sets it to false.

## Data attribution

Train data comes from [Fintraffic Digitraffic](https://www.digitraffic.fi/en/) and is licensed under [Creative Commons Attribution 4.0 International (CC BY 4.0)](https://creativecommons.org/licenses/by/4.0/). This service modifies the data: it derives a current delay from the most recently reached timetable stop and compares it with user-defined thresholds. Alerts are a derived work.

`GET /api/alerts` returns the attribution with every response, so clients of the API carry it too. No API key is required or used. Requests to Digitraffic identify the caller with the `Digitraffic-User` header, as Fintraffic asks.
