# Working in this repo

## The invariant

Duplicate alerts are prevented by `UNIQUE (rule_id, train_number, departure_date)`
in `migrations/00001_init.sql`, not by application code. `InsertAlerts` uses
`ON CONFLICT DO NOTHING ... RETURNING` and treats the rows the database accepted
as the new ones.

Do not replace this with a "check then insert" in Go. The poller can overlap with
itself — cron does not wait for the previous run — and a read-then-write lets two
runs both conclude an alert is new. If you change the key, change it knowing:

- **the delay is excluded** so a worsening train does not re-fire every minute
- **the departure date is included** because the same train number runs daily
- **the rule id is included** so two users watching one train both get told

Each of those is a named test in `internal/store/store_test.go`.

## Testing

- `internal/rules` is pure. Keep it that way: no database, no HTTP, no clock reads
  that are not passed in.
- `internal/store` tests run against real Postgres because the behaviour under
  test *is* the schema. They skip without `DATABASE_URL`; CI supplies one.
  Locally: `docker run -d -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=fa -p 5432:5432 postgres:16-alpine`
- `internal/api` and `internal/poll` declare narrow interfaces at the consumer and
  test against fakes. Do not widen those interfaces to the full `*store.Store`.

## Security invariants

- **Webhook URLs are attacker input.** `webhook.Validate` rejects non-http schemes
  and literal private addresses; the dialer's `Control` hook re-checks the resolved
  IP; redirects are never followed. All three are needed — removing any one
  reopens SSRF. `AllowPrivateAddresses` exists only so tests can dial httptest;
  never set it outside a test.
- **Sessions store a hash, never the token.** The cookie carries the raw value.
- **OAuth requests no scopes.** Sign-in learns who the user is and gains access to
  nothing in their account. Do not add scopes without a concrete need.
- Rule and alert queries are scoped by `user_id`. A missing scope is a data leak,
  not a bug — `TestOneUserCannotDeleteAnothersRule` guards the delete path.
- **`ALLOWED_LOGINS` is the whole access policy**, and it works because the check
  sits in the OAuth callback: every authenticated route requires a session, and
  `callback` is the only place a session is minted. Keep it that way — a new
  route that creates a session another way would bypass it silently. The refusal
  must stay *before* `UpsertUser` so a rejected sign-in leaves no user row;
  `TestARefusedLoginGetsA403AndCreatesNoUser` pins that ordering.
- **An unset allowlist is open, deliberately** — a hard-coded list would make the
  repo useless to anyone who cloned it. That default is load-bearing for a public
  repo, so `serve` warns at startup when it applies. If you ever make it
  fail-closed, delete that warning too, or it will lie.
- **Do not add a way to repoint `auth.GitHub`'s OAuth URLs from outside the
  package.** The `api` package depends on the `OAuth` interface it declares, so
  the sign-in path is testable with a fake and the production type needs no
  seam. An exported setter would be a way to send client credentials to another
  host, in exchange for nothing.

## Data licence

Train data is Fintraffic Digitraffic under CC BY 4.0. Attribution is required and
lives in three places: the README, the `dataAttribution` constant returned by
`GET /api/alerts`, and the `Digitraffic-User` request header. Do not remove them.

Do **not** set `Accept-Encoding` by hand in the Digitraffic client — Go's transport
sets it and decompresses transparently, and Digitraffic answers 406 without gzip.
`TestDoesNotSetAcceptEncodingItself` pins this.

## Running things

- `go test ./...` — store tests skip without `DATABASE_URL`
- `./freightalerts migrate | poll | serve`
- The poller is a single pass, not a daemon. The schedule lives in
  `.github/workflows/poll.yml`. Keep it that way: it is what lets the service run
  with no always-on host.
