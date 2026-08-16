# Deploying

Three things live in your accounts rather than this repo: a database, a host for
the API, and a GitHub OAuth app. They have to happen in that order, because the
OAuth app needs a callback URL that only exists once the API is running.

**The poller works after step 1 alone.** Steps 2 and 3 only add the web surface —
so if all you want is alerts landing in a database on a schedule, stop after the
first section.

---

## 1. Database — the poller starts working here

Any managed Postgres works. Neon's free tier suits this shape: nothing runs
between cycles, so the compute sits suspended and costs nothing.

1. Create a project at [neon.tech](https://neon.tech) and copy the connection
   string. It looks like
   `postgres://user:pass@ep-xxx.region.aws.neon.tech/neondb?sslmode=require`.
2. Apply the schema:

   ```bash
   DATABASE_URL='<connection string>' ./freightalerts migrate
   ```

3. Add it as a repository secret so the scheduled poll can reach it:
   **Settings → Secrets and variables → Actions → New repository secret**,
   named `DATABASE_URL`.

The poll workflow checks for that secret and skips cleanly without it, so until
this point the cron has been running and doing nothing rather than failing every
fifteen minutes.

**Verify:** trigger it manually — **Actions → Poll → Run workflow** — and look for
`poll complete` in the log with a train count. Then:

```sql
SELECT id, trains_seen, alerts_created, error FROM poll_runs ORDER BY id DESC LIMIT 5;
```

A row per run, with `error` null. No alerts yet is correct: nobody has a rule.

> ⚠️ **Keep the repository public.** At 96 runs a day the poll would consume
> roughly 36 hours of Actions time a month — free for public repos, far beyond
> the 2,000-minute allowance for private ones.

> GitHub disables scheduled workflows on repositories with no activity for 60
> days. If alerts stop arriving after a quiet period, check that first.

---

## 2. Host the API

The API is a stateless container that reads `PORT`. Any of these work; Cloud Run
is the closest to free-and-not-sleeping, and you already have GCP from
`data-engineer-finance-analytics`.

### Google Cloud Run

```bash
gcloud run deploy freight-alerts \
  --source . \
  --region europe-north1 \
  --allow-unauthenticated \
  --set-env-vars "DATABASE_URL=<connection string>"
```

Scales to zero, so an idle service costs nothing; expect roughly a second of cold
start on the first request after a quiet period. It needs a billing account
attached even to stay inside the free tier — **set a budget alert** before you
walk away from it.

Note the service URL it prints. That is your `GITHUB_REDIRECT_URL` base.

### Alternatives

| Host | Cost | Trade-off |
|---|---|---|
| Fly.io / Railway | ~$5/mo | Always warm, no cold start |
| Render free tier | Free | Spins down; ~30s cold start on the demo |

**Verify:** `curl https://<your-host>/healthz` returns `{"status":"ok"}`.

---

## 3. GitHub OAuth app — sign-in

Until this exists, `/auth/login` returns a clear 503 and everything else works.

1. **Settings → Developer settings → OAuth Apps → New OAuth App**
   - Homepage URL: your service URL
   - **Authorization callback URL**: `https://<your-host>/auth/callback` — this
     must match `GITHUB_REDIRECT_URL` exactly, including the scheme and path
2. Generate a client secret.
3. Set all three on the service:

   ```bash
   gcloud run services update freight-alerts --region europe-north1 \
     --set-env-vars "GITHUB_CLIENT_ID=...,GITHUB_CLIENT_SECRET=...,GITHUB_REDIRECT_URL=https://<your-host>/auth/callback"
   ```

**Verify:** open `https://<your-host>/auth/login` in a browser. You should land on
GitHub, be asked to authorise, and come back to a JSON body with your login. The
consent screen should request **no permissions** — that is deliberate; the service
learns who you are and gains access to nothing.

---

## 4. Prove it end to end

```bash
# Sign in first so the browser holds a session, then from that browser's cookies:
curl -X POST https://<your-host>/api/rules \
  -H 'Content-Type: application/json' \
  -b 'fa_session=<session cookie>' \
  -d '{"kind":"operator","target":"vr","threshold_minutes":10}'

# Trigger a poll (Actions → Poll → Run workflow), then:
curl https://<your-host>/api/alerts -b 'fa_session=<session cookie>'
```

Finnish cargo rail is quiet overnight (UTC+2/+3) — if nothing fires, check
`poll_runs.trains_seen` before assuming a bug. Zero trains at 03:00 local is the
service working correctly.

---

## 5. A device token, for the Flutter client

Browsers use the session cookie. Native clients can't, so mint a bearer token:

```bash
curl -X POST https://<your-host>/api/tokens -b 'fa_session=<session cookie>'
```

```json
{ "token": "…", "expires_at": "…", "note": "Shown once. Send it as: Authorization: Bearer <token>" }
```

It is a session row like any other — same hashing, same expiry, revoked the same
way. The plaintext is shown exactly once because the server keeps only the hash.

---

## Rotating and revoking

- **Database URL:** rotate in Neon, update the repo secret and the host's env.
- **OAuth secret:** regenerate on GitHub, update the host's env. Existing sessions
  survive — they don't depend on it.
- **A leaked device token:** delete its row.
  `DELETE FROM sessions WHERE token_hash = decode('<sha256 hex>', 'hex');`
  You can't look the token up by its plaintext — that is the point — so if you
  don't know which row it is, `DELETE FROM sessions WHERE user_id = <id>` revokes
  every session for that user and signs them out everywhere.
