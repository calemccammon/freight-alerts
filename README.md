# Freight Alerts

Delay alerts for Finnish cargo rail. Users sign in with GitHub, register watch rules, and are notified when a matching train runs late. It is a Go service over Postgres that polls the keyless [Digitraffic](https://www.digitraffic.fi/en/) open API and sends one alert per rule, train and departure date, however many times the poller runs.

```
cron -> poll Digitraffic -> evaluate rules -> insert (deduplicated by the database) -> webhook + feed
```

There is no live instance at present. The service ran on Google Cloud Run with a scheduled GitHub Actions poller from 2026-08-16 to 2026-10-15. See [Deployment](#deployment) for what was deployed and why it stopped.

## Getting started

Go 1.25 and a Postgres database are required.

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

The binary has three subcommands: `migrate`, `poll` and `serve`. `poll` and `serve` both apply pending migrations before they start, so `migrate` is only needed on its own to prepare a database ahead of time.

| Variable | Used by | Meaning |
|---|---|---|
| `DATABASE_URL` | all | Postgres connection string. Required. |
| `PORT` | `serve` | Listen port. Defaults to 8080. |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `GITHUB_REDIRECT_URL` | `serve` | GitHub OAuth app credentials and callback URL. |
| `ALLOWED_LOGINS` | `serve` | Comma-separated GitHub logins permitted to sign in. Unset means any GitHub account may. |
| `CORS_ORIGINS` | `serve` | Comma-separated browser origins allowed to call the API cross-origin. Unset means none. |
| `INSECURE_COOKIES` | `serve` | Set to `1` to drop the cookie `Secure` flag for local http. |

Without the three OAuth variables the service still starts. The poller does not need them, and `/auth/login` returns a 503 with a JSON error.

A `Dockerfile` is included. It is a multi-stage build that produces a static binary on a distroless base image and runs as a non-root user.

```bash
docker build -t freight-alerts .
docker run -p 8080:8080 -e DATABASE_URL=... freight-alerts        # serve
docker run -e DATABASE_URL=... freight-alerts poll                # one cycle
```

## Architecture

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
              │ Postgres          │◀──── freightalerts serve
              └───────────────────┘         GitHub OAuth,
                                            rule CRUD, alert feed
```

The poller is a single pass that exits. The schedule lives outside the process, in `.github/workflows/poll.yml`, so nothing has to stay running between cycles. One cycle does the following:

1. Records a row in `poll_runs`.
2. Loads the active rules. If there are none it stops without calling Digitraffic.
3. Fetches the cargo trains currently running from the Digitraffic GraphQL endpoint.
4. Evaluates every rule against every train. A train's delay is the delay at the most recent stop it has actually reached. Estimates for future stops are ignored, and so is a delay earlier in the journey that the train has since recovered.
5. Inserts the candidate alerts and keeps the rows the database accepted.
6. Posts a webhook for each accepted row whose rule has a webhook URL, and records the delivery outcome on the alert.
7. Updates the `poll_runs` row with the counts and any error.

The code is organised by package:

| Package | Role |
|---|---|
| `cmd/freightalerts` | Subcommand dispatch, environment configuration, HTTP server lifecycle |
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
| `POST` | `/auth/logout` | delete the cookie's session row and clear the cookie |
| `GET` | `/api/me` | current user |
| `POST` | `/api/tokens` | mint a bearer token for a non-browser client. The plaintext is returned once. |
| `GET` | `/api/rules` | the caller's rules |
| `POST` | `/api/rules` | create a rule |
| `DELETE` | `/api/rules/{id}` | delete one of the caller's rules. Another user's id returns 404. |
| `GET` | `/api/alerts` | the caller's alerts, newest first. `limit` defaults to 100, maximum 500. |

```jsonc
// POST /api/rules
{
  "kind": "operator",        // "train" | "operator" | "station"
  "target": "vr",            // train number, operator code, or station name
  "threshold_minutes": 15,   // fires at >= this many minutes late, 1 to 1440
  "webhook_url": "https://hooks.example.com/x"   // optional
}
```

Submitting a rule with the same kind, target and threshold as an existing one returns the existing rule's id and replaces its webhook URL. It does not create a second subscription. A `train` rule matches the train number on every departure date.

Every `/api/` route requires a session. Browsers send it as the `fa_session` cookie, and other clients send the same kind of token as `Authorization: Bearer <token>`. A token from `/api/tokens` is a row in `sessions` like a browser session, hashed the same way and expiring after the same 30 days.

`GET /api/alerts` returns an `attribution` field with the data licence notice alongside the alerts.

The Flutter app [`flutter-freight-corridor`](https://github.com/calemccammon/flutter-freight-corridor) is a client of this API. It registers its watchlist as rules using a bearer token.

## Design decisions

### Duplicate suppression is a database constraint

A train that is 20 minutes late is still late on the next poll, so a poller that notifies on what it finds would notify on every cycle. Checking for an existing alert and then inserting does not fix this safely, because two overlapping runs can both pass the check before either inserts.

The check is therefore a constraint on `alerts`:

```sql
UNIQUE (rule_id, train_number, departure_date)
```

`InsertAlerts` uses `INSERT ... ON CONFLICT DO NOTHING RETURNING id` and treats the rows the database returned as the new alerts. Only those rows get a webhook. A run that overlaps another loses the conflict and sends nothing.

The columns in the key were chosen for these reasons, and each has a test in `internal/store/store_test.go`:

- The delay is excluded. With the delay in the key, a train slipping from 20 to 21 minutes late would alert again.
- The departure date is included. The same train number runs daily, and without the date the first alert would suppress every later day's.
- The rule id is included, so two users watching the same train are both notified.

### Webhook URLs are treated as hostile input

A URL supplied by a user and fetched by the server is a server-side request forgery vector. `internal/webhook` applies three checks:

- At rule creation, the URL must be `http` or `https` with a host, and a literal loopback, private, link-local, multicast or unspecified address is rejected with a 400.
- A hostname that resolves to such an address cannot be caught at creation. The HTTP client's dialer has a `Control` hook that inspects the IP actually being connected to, after DNS resolution, and refuses non-public addresses. This also covers a DNS record that changes after the rule was created.
- Redirects are not followed, so a public URL cannot forward the request to an internal address.

The transport sets no proxy, so the dial-time check sees the real destination.

### Sessions and OAuth

- A session token is 32 random bytes. The database stores only its SHA-256, so a copy of the `sessions` table does not contain usable tokens. The hash is unsalted and fast because the input is already high-entropy.
- The session cookie is `HttpOnly`, `SameSite=Lax` and `Secure` unless `INSECURE_COOKIES=1`.
- Logout deletes the session row as well as clearing the cookie.
- The OAuth `state` value is generated per sign-in attempt, held in a 10-minute `HttpOnly` cookie, compared in constant time on callback, and the cookie is cleared whether or not the comparison succeeds.
- The authorization request asks for no scopes. The service reads the user's id and login and receives no access to the account.
- The OAuth flow is written directly against `net/http` in `internal/auth/github.go`. For one provider it is two requests.

### Access policy

`ALLOWED_LOGINS` is checked in the OAuth callback, before the user row is created. Every authenticated route requires a session, and the callback is the only place a browser session is created, so that one check covers the whole API. Logins are compared case-insensitively. An unset or empty value leaves sign-in open so that a fresh clone works without configuration, and `serve` logs a warning at startup when that is the case.

`CORS_ORIGINS` has the opposite default: unset permits no cross-origin access. A permitted origin is echoed back in `Access-Control-Allow-Origin` instead of a wildcard, and `Access-Control-Allow-Credentials` is never sent. Browsers therefore do not attach the session cookie to cross-origin requests, and a cross-origin client has to use a bearer token.

### Migrations

Migrations are SQL files embedded in the binary and applied by about a hundred lines in `internal/store/migrate.go`. Each migration runs in its own transaction, and a Postgres advisory lock serialises instances that start at the same time. They are forward-only. The runner is hand-written because goose and golang-migrate bring in drivers for databases this service does not use. The only direct dependency in `go.mod` is `pgx`.

### Interfaces declared by the consumer

`internal/api` and `internal/poll` each declare the store methods they need as an interface, and `internal/api` does the same for the OAuth provider. Their tests run against in-memory fakes and need no database.

### Poll bookkeeping

Each cycle writes a `poll_runs` row with its counts and any error, which distinguishes a poller that has stopped from one that found nothing to report. A failed webhook is logged and recorded on the alert as `webhook_status`, and the cycle continues with the remaining alerts.

## Deployment

[`DEPLOY.md`](DEPLOY.md) is the runbook. It covers the database, the API host and the GitHub OAuth app, in the order they have to be set up.

The poller needs no host. `.github/workflows/poll.yml` runs `freightalerts poll` on a `*/15` cron with `DATABASE_URL` as a repository secret. If the secret is missing the workflow skips the poll step and succeeds.

The API is a stateless container that reads `PORT`. It was deployed to Google Cloud Run in `us-east5`, configured to scale to zero. Cloudflare Workers was considered first and rejected because it does not run a Go binary.

Status of that deployment:

- The scheduled workflow ran from 2026-08-16 to 2026-10-15: 694 scheduled runs, of which 688 succeeded. GitHub then disabled the schedule automatically, which it does after 60 days without repository activity. Re-enabling it is a manual step on the Actions tab.
- The Cloud Run URL no longer answers `/health` successfully.

Two things learned from the deployment:

- The liveness route is `/health`. On Cloud Run, `/healthz` was answered by Google's front end with a 404 and never reached the container.
- The region should be near Postgres. The API never calls Digitraffic, and both the API and the Actions runners talk to the database.

## Tests

There are 141 test functions across seven packages. None of them calls an external service.

| Package | Tests | What they run against |
|---|---|---|
| `internal/rules` | 23 | Pure functions |
| `internal/store` | 21 | A real Postgres, because the behaviour under test is the schema. They skip when `DATABASE_URL` is unset. |
| `internal/api` | 43 | In-memory fakes of the store and OAuth provider |
| `internal/poll` | 12 | In-memory fakes of the fetcher, store and notifier |
| `internal/auth` | 20 | `httptest` servers standing in for GitHub |
| `internal/webhook` | 12 | `httptest` servers on loopback |
| `internal/digitraffic` | 10 | `httptest` servers standing in for Digitraffic |

CI (`.github/workflows/ci.yml`) starts a Postgres 16 service container, checks `gofmt`, runs `go vet` and `go test -race ./...`, builds the binary and runs `migrate` against the database.

The suppression tests insert the same alert twice in sequence. No test runs two polls concurrently.

## Limitations

- Rail only. Digitraffic also publishes marine AIS and port calls, which this service does not read.
- Webhooks are not retried. A failed delivery is recorded on the alert and the alert stays in the feed. If the process dies between the insert and the webhook, that alert is never delivered.
- There is no rate limiting on the API or on rule creation.
- Notification latency is bounded by the schedule, and GitHub's cron is best-effort. In the 60 days the workflow ran, the `*/15` schedule produced 694 runs, about one every two hours.
- A bearer token cannot be revoked through the API. `POST /auth/logout` deletes only the session in the cookie. Revoking a token means deleting its row, as described in `DEPLOY.md`.
- Expired sessions are purged at the end of a poll cycle, and only on cycles that produced at least one candidate alert.
- The webhook address check covers loopback, RFC 1918, link-local, multicast, unspecified and IPv6 unique-local addresses. Other reserved ranges, such as 100.64.0.0/10, are not blocked.
- Rules cannot be paused through the API. The `active` column exists, but no endpoint sets it to false.

## Licence and data attribution

The code is released under the MIT licence.

Train data comes from [Fintraffic Digitraffic](https://www.digitraffic.fi/en/) and is licensed under [Creative Commons Attribution 4.0 International (CC BY 4.0)](https://creativecommons.org/licenses/by/4.0/). This service modifies the data: it derives a current delay from the most recently reached timetable stop and compares it with user-defined thresholds. Alerts are a derived work.

`GET /api/alerts` returns the attribution with every response, so clients of the API carry it too. No API key is required or used. Requests to Digitraffic identify the caller with the `Digitraffic-User` header, as Fintraffic asks.
