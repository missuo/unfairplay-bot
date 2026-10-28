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
- **One live message per task.** The confirmation message turns into a
  progress view: queue position, download bar with speed and ETA, then
  decryption, then a download button. Users can cancel their own tasks;
  admins can cancel any.
- **Allowlist.** Admins always have access. Anyone else gets their Telegram
  ID and a "no access" reply, and admins receive an access request with
  **✅ 允许** / **🚫 忽略** buttons (at most once per 6 h per person). Admin
  commands: `/allow <id> [note]`, `/revoke <id>`, `/users` (with one-tap
  removal), and `/status` (Apple Account, queue, cache, versions, active
  tasks). The allowlist is stored in `data/telegram-allowlist.json`.
- **Admin notices.** Every request by a non-admin (new task, joined an
  in-progress task, or cache hit) and every completion, failure, or
  cancellation is sent to the admins. Notices use a `🔔 通知 · …` header and a
  quoted block, so they look different from replies to the admin's own
  requests. The admin's own requests produce no notices. If the Apple session
  stops working, admins get a re-login alert (at most hourly).

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

# Log in to ipatool once, inside the running container, with the same Apple
# Account that produced the UnfairPlay model.
docker compose exec -it bot ipatool auth login --email you@example.com \
  --keychain-passphrase "$UNFAIRPLAY_BOT_KEYCHAIN_PASSPHRASE"
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
| Created by | `unfairplay setup` on an enrolled Mac | `ipatool auth login` (Apple ID, password, 2FA code) |
| Portable to another machine | **Yes.** Copy the file securely; UnfairPlay supports Linux decryption with a copied model | **No.** On macOS it lives in the system Keychain; log in again on the server |
| Expiry | None in UnfairPlay | The App Store token expires; the bot re-logs in automatically with the saved password |

Both must belong to the same Apple Account: the model decrypts IPAs whose
licenses carry that account's FairPlay user and key selector.

## Automatic dependency updates

`deps.env` sets how each dependency is tracked: `main`, `release`, or
`prerelease`. ipatool is pinned in `go.mod`; UnfairPlay in `deps.env`.

1. `.github/workflows/update-deps.yml` runs every 6 hours. It resolves the
   newest allowed versions (`scripts/update-deps.sh`), runs vet and race
   tests, builds both CLIs, commits the bump, and publishes
   `ghcr.io/<owner>/unfairplay-bot:latest`.
2. On the server, `deploy/unfairplay-bot-update.timer` runs
   `deploy/update.sh` hourly. It pulls the image and, if a personalized
   encrypted IPA is in `./canary`, runs `unfairplay verify` with the private
   model. If verification fails, it keeps the old version. Otherwise it waits
   for the bot to be idle and restarts it. The bot also drains queued and
   running tasks on SIGTERM.

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
