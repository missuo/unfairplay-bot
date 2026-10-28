# unfairplay-bot

A private Telegram bot and HTTP service that takes an App Store `bundle_id`
or `app_id`,
downloads the build the App Store serves right now with
[ipatool](https://github.com/majd/ipatool), decrypts it with
[UnfairPlay](https://github.com/DrWhax/unfairplay), and returns a signed,
expiring download link.

Use it only with apps your account lawfully acquired. Decrypted IPAs and the
links to them are private.

## How it works

- **Tasks.** `POST /v1/tasks` creates a task. Up to 3 tasks run at once
  (`UNFAIRPLAY_BOT_CONCURRENCY`); the rest wait in FIFO order and report their
  queue position. Decryption is limited separately
  (`UNFAIRPLAY_BOT_DECRYPT_CONCURRENCY`, default 1).
- **Progress.** Stages are `queued → downloading → patching →
  waiting_decrypt → decrypting → finalizing → ready` (or `failed` /
  `canceled`). While downloading, the task reports bytes, total, percent,
  speed, and ETA. Follow it with `GET /v1/tasks/{id}/events` (Server-Sent
  Events) or poll `GET /v1/tasks/{id}`.
- **Cache hits only for the current build.** Each request first asks the App
  Store download endpoint for the latest external version ID, so it does not
  depend on the iTunes lookup API, which can lag behind a release. The cache
  key is `trackId:externalVersionId`. When the App Store moves from 8.0.79 to
  8.0.80, the next request misses and downloads 8.0.80. Requests by
  `bundle_id` and by `app_id` for the same app share the cache.
- **Deduplication.** Submitting a build that is already queued or running
  returns the existing task.
- **Expiry.** An artifact lives 24 h (`UNFAIRPLAY_BOT_CACHE_TTL`) after its
  most recent hit. Hits are API cache hits and full downloads. A signed link
  stays valid until the artifact's expiry at the time the link was issued;
  request the app again to get a fresh link. `UNFAIRPLAY_BOT_MAX_CACHE_BYTES`
  optionally caps disk use, evicting the least recently hit artifacts first.

## Telegram bot

Set `UNFAIRPLAY_BOT_TELEGRAM_TOKEN` and `UNFAIRPLAY_BOT_TELEGRAM_ADMIN_IDS`.
The bot uses long polling, so it needs no public webhook. It only answers in
private chats.

- **Confirm first.** Send an App ID, a bundle ID, or an App Store link. The
  bot replies with the name, developer, current version and release date,
  size, price, and whether that build is already cached. Nothing is queued
  until the user taps **✅** within 10 minutes.
- **Search.** `/search <name>` lists up to 10 matches from the Apple ID's
  storefront as buttons; tapping one opens the same preview. Neither search
  nor a preview adds an app to the Apple ID; that happens only after **✅**.
- **One live message per task.** The confirmation message turns into a
  progress view: queue position, download bar with speed and ETA, then
  decryption, then a download button. Users can cancel their own tasks;
  admins can cancel any.
- **Allowlist.** Admins always have access. Anyone else gets their Telegram
  ID and a "no access" reply, and admins receive an access request with
  **✅ Allow** / **🚫 Ignore** buttons (at most once per 6 h per person). Admin
  commands: `/allow <id> [note]`, `/revoke <id>`, `/users` (with one-tap
  removal), `/status` (Apple Account, queue, cache, versions, active
  tasks), `/health` (what `ipatool auth info` shows, the session and
  automatic sign-in state, and a live App Store request), and `/clear`
  (after a confirmation, delete every cached IPA regardless of expiry; issued
  links stop working, tasks in progress are unaffected). The allowlist is stored in `data/telegram-allowlist.json`.
- **Admin notices.** Every request by a non-admin (new task, joined an
  in-progress task, or cache hit) and every completion, failure, or
  cancellation is sent to the admins. Notices use a `🔔 Notice · …` header and a
  quoted block, so they look different from replies to the admin's own
  requests. The admin's own requests produce no notices. If the Apple session
  stops working, see "App Store health" below.
- **`/login` (admins only).** Signs the App Store account in again with the
  configured (or saved) credentials. If Apple asks for a 2FA code, only the
  admin who sent `/login` can answer it, within 10 minutes, by replying with
  the 6 digits.

## API

All `/v1` endpoints require `Authorization: Bearer $TOKEN`, or `?token=` for
EventSource clients, which cannot set headers.

```sh
# Create (or reuse) a task.
curl -s -H "Authorization: Bearer $TOKEN" -d '{"bundle_id":"com.example.app"}' \
  https://ipa.example.com/v1/tasks
# Body may also be {"app_id": 123456789}.

# Follow progress live.
curl -N -H "Authorization: Bearer $TOKEN" https://ipa.example.com/v1/tasks/$ID/events
```

| Response | Meaning |
|---|---|
| `200 {"cached": true, "artifact": {...}}` | Current build is cached; `artifact.download_url` is ready |
| `201 {"cached": false, "task": {...}}` | New task queued |
| `200 {"cached": false, "task": {...}}` | Same build already in progress; existing task returned |
| `429 queue_full` | More than `CONCURRENCY + MAX_QUEUE` tasks |
| `503 auth_required` | ipatool session unusable; log in again (see below) |
| `403 license_required` | Paid app not owned by the account |

Each SSE event is `event: task` with the full task JSON. The stream ends after
`ready`, `failed`, or `canceled`:

```json
{"id":"…","status":"downloading","app":{"app_id":123,"bundle_id":"com.example.app","external_version_id":"870123456"},
 "progress":{"bytes":734003200,"total":2147483648,"percent":34.2,"speed_bps":18500000,"eta_seconds":76}}
```

A `ready` task contains `artifact.download_url`. Other endpoints:
`GET /v1/tasks`, `DELETE /v1/tasks/{id}` (cancel), `GET /v1/status` (queue,
cache, dependency versions), `GET /healthz`. Download links (`/d/...`) need no
token and support `Range` requests.

## Deploy (Docker Compose)

```sh
mkdir -p /opt/unfairplay-bot && cd /opt/unfairplay-bot
# copy compose.yaml, .env.example and deploy/ from this repository
cp .env.example .env            # fill in the values
mkdir -p data ipatool cache secrets/models canary
cp /path/to/your-model.bin secrets/models/
sudo chown -R 10001:10001 data ipatool cache secrets
# UnfairPlay refuses group/world-readable models and model directories.
sudo chmod 700 secrets/models && sudo chmod 600 secrets/models/*.bin
docker compose up -d

# With UNFAIRPLAY_BOT_APPLE_ID_* set, the service signs in by itself. For a
# 2FA account, send /login to the bot, or sign in inside the container:
# docker compose exec -it bot ipatool auth login --email you@example.com \
#   --keychain-passphrase "$UNFAIRPLAY_BOT_KEYCHAIN_PASSPHRASE"
```

`secrets/models` is UnfairPlay's model registry: it may hold several `.bin`
models, and UnfairPlay picks the one matching each IPA. Add a model by copying
another `.bin` file there; no restart is needed.

Put a reverse proxy with TLS in front of `127.0.0.1:8080`. For SSE, disable
response buffering (the service sends `X-Accel-Buffering: no` for nginx).

The compose file pins the container MAC address, because ipatool derives the
App Store device GUID from it. Do not change it after logging in.

## Credentials

The service needs two separate private credentials:

| | UnfairPlay model (`.bin`) | ipatool session |
|---|---|---|
| Purpose | Decrypts IPAs | Signs in to the App Store to download them |
| Created by | `unfairplay setup` on an enrolled Mac | The service itself, from `UNFAIRPLAY_BOT_APPLE_ID_EMAIL` / `_PASSWORD`, or `ipatool auth login` |
| Portable to another machine | **Yes.** Copy the file securely; decryption is offline | **No.** On macOS it lives in the system Keychain; sign in again on the server |
| Expiry | Never | The App Store token expires; the service signs in again automatically |

Both must belong to the same Apple ID: the model decrypts every IPA that
account downloads.

**Automatic sign-in** (accounts without 2FA): with the Apple ID set in `.env`,
the service signs in on first start and whenever the session is missing,
revoked, or expired. Failed automatic attempts are at least 15 minutes apart,
and after `UNFAIRPLAY_BOT_AUTO_LOGIN_MAX_ATTEMPTS` (default 3) consecutive
failures automatic sign-in stops, so a wrong password or a login flow Apple no
longer accepts cannot get the account locked. The count survives restarts
(`data/auto-login.json`) and resets after any successful sign-in; a manual
`/login` is always allowed.

## App Store health

The likeliest outage is on the App Store side: a lost session, or Apple
changing the login or download API under ipatool. With
`UNFAIRPLAY_BOT_HEALTH_APP_ID` set to a free app the account owns, the
service asks the App Store for that app's latest version every
`UNFAIRPLAY_BOT_HEALTH_INTERVAL` (30 min). That is the same request a
download starts with, minus the file transfer. Any task or preview that fails
on the App Store side triggers an immediate re-check.

Admins get one notice when the check starts failing (with the error and what
to do: `/login` for a session problem, or wait for the automatic ipatool
update for an API change) and one when it recovers. `/status` and
`GET /v1/status` show the current state.

## Deploy (systemd, no Docker)

For hosts where Docker is unwanted (for example a router whose iptables
rules must not change), `deploy/systemd/` builds and runs the service
natively. It needs Go (the version in `go.mod` or newer), gcc, git, and
Caddy.

```sh
useradd --system --home-dir /var/lib/unfairplay-bot --shell /usr/sbin/nologin unfairplay
install -d -o unfairplay -g unfairplay -m 700 /var/lib/unfairplay-bot/{,data,models,canary}
install -m 600 your-model.bin /var/lib/unfairplay-bot/models/ && chown unfairplay: /var/lib/unfairplay-bot/models/*
install -d -m 700 /etc/unfairplay-bot     # put the settings from .env.example in /etc/unfairplay-bot/env (0600)
git clone https://github.com/missuo/unfairplay-bot /opt/unfairplay-bot/src
cp /opt/unfairplay-bot/src/deploy/systemd/*.service /opt/unfairplay-bot/src/deploy/systemd/*.timer /etc/systemd/system/
systemctl daemon-reload && systemctl enable unfairplay-bot unfairplay-bot-update.timer
/opt/unfairplay-bot/src/deploy/systemd/update.sh   # first build and start
systemctl start unfairplay-bot-update.timer
```

`update.sh` runs hourly: it builds the newest `main` (which includes the
CI's dependency bumps), runs the tests, then the same two canaries as the
Docker updater before switching the `current` symlink and restarting while
idle. Serve it with `deploy/systemd/Caddyfile`.

## Automatic dependency updates

`deps.env` sets how each dependency is tracked: `main`, `release`, or
`prerelease`. ipatool is pinned in `go.mod`; UnfairPlay in `deps.env`.

1. `.github/workflows/update-deps.yml` runs every 6 hours. It resolves the
   newest allowed versions (`scripts/update-deps.sh`), runs vet and race
   tests, builds both CLIs, commits the bump, and publishes
   `ghcr.io/<owner>/unfairplay-bot:latest`.
2. On the server, `deploy/unfairplay-bot-update.timer` runs
   `deploy/update.sh` hourly. It pulls the image and runs two canaries with
   the new image before switching:
   - **App Store:** `unfairplay-bot check` with the existing session, inside
     the running container's network namespace so the MAC address (ipatool's
     device identity) is the same. It never signs in. If it fails while the
     running version is healthy, the update is rejected.
   - **UnfairPlay:** `unfairplay verify` on an IPA in `./canary` with the
     private model, offline.

   If both pass, the updater waits for the bot to be idle, then restarts it.
   If the running version is already unhealthy, it restarts immediately. The
   bot also drains queued and running tasks on SIGTERM.

```sh
sudo cp deploy/unfairplay-bot-update.* /etc/systemd/system/
sudo systemctl enable --now unfairplay-bot-update.timer
```

Public CI cannot test real downloads or decryption, because they need the
private session and model. The canary check is the gate for those.

## Development

```sh
go vet ./... && go test -race ./...
```

The Linux build needs cgo (ipatool's keyring dependency), so build on Linux or
with the Dockerfile rather than cross-compiling from macOS.

## License

Apache License 2.0; see [LICENSE](LICENSE). The Docker image also contains
[ipatool](https://github.com/majd/ipatool) and
[UnfairPlay](https://github.com/DrWhax/unfairplay), which remain under their
own licenses.
