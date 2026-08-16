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
  --region us-east5 \
  --allow-unauthenticated \
  --min-instances=0 \
  --set-secrets "DATABASE_URL=freight-alerts-db:latest"
```

**Put the region near your database, not near Digitraffic.** This service never
calls Digitraffic — only the poller does, and that runs on GitHub Actions
runners. So the API's one latency-sensitive dependency is Postgres, and the
runners' is too. `us-east5` (Columbus) sits beside Neon's `us-east-2` and close
to the Actions pool.

`--min-instances=0` is stated explicitly rather than left to the default,
because it is the setting that keeps this inside the free tier. Setting it to 1
keeps a container warm around the clock — roughly 2.6M vCPU-seconds a month
against a 180,000 allowance, and the usual way people get a surprise Cloud Run
bill. A one-second cold start is the right trade here.

`--set-secrets` rather than `--set-env-vars` keeps the connection string out of
`gcloud run services describe`, out of the console, and out of every revision's
history. Create it once with
`gcloud secrets create freight-alerts-db --data-file=-`, then grant the runtime
service account `roles/secretmanager.secretAccessor` **on that secret** rather
than project-wide.

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

**Verify:** `curl https://<your-host>/health` returns `{"status":"ok"}`.

---

## 3. GitHub OAuth app — sign-in

Until this exists, `/auth/login` returns a clear 503 and everything else works.

1. **Settings → Developer settings → OAuth Apps → New OAuth App**
   - Homepage URL: your service URL
   - **Authorization callback URL**: `https://<your-host>/auth/callback` — this
     must match `GITHUB_REDIRECT_URL` exactly, including the scheme and path
2. Generate a client secret.
3. Set them on the service, together with the allowlist:

   ```bash
   gcloud run services update freight-alerts --region us-east5 \
     --set-env-vars "GITHUB_CLIENT_ID=...,GITHUB_CLIENT_SECRET=...,GITHUB_REDIRECT_URL=https://<your-host>/auth/callback,ALLOWED_LOGINS=<your-github-login>"
   ```

   **Do not skip `ALLOWED_LOGINS` on a public deployment.** Unset, it means any
   GitHub account may sign in and create rules that your poller then evaluates
   every fifteen minutes — no access to your data, since every query is scoped
   by user, but an open door to your compute and to outbound webhook delivery,
   neither of which is rate limited. Set it to a comma-separated list of the
   logins that should be able to sign in.

   The default is open on purpose so that cloning this repository yields a
   working service with no configuration. That makes it your deployment's job,
   not the code's, to close it — so `serve` logs which mode it is in at
   startup. Look for one of these in the logs and confirm you got the one you
   meant:

   ```
   level=WARN msg="ALLOWED_LOGINS is not set; sign-in is open to any GitHub account"
   level=INFO msg="sign-in restricted" permitted_logins=1
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
