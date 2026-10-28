#!/usr/bin/env bash
# Server-side updater, run by unfairplay-bot-update.timer from the compose
# directory. Pulls the newest image, proves it can still decrypt with the
# private model, waits for the bot to go idle, then restarts it. A failed
# canary keeps the running version.
set -euo pipefail
cd "$(dirname "$0")/.."

image=$(docker compose config --images | head -1)
old=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
docker compose pull --quiet bot
new=$(docker image inspect --format '{{.Id}}' "$image")
if [[ "$new" == "$old" ]]; then
	echo "already up to date"
	exit 0
fi

canary=$(find canary -maxdepth 1 -name '*.ipa' 2>/dev/null | head -1 || true)
if [[ -n "$canary" ]]; then
	echo "verifying new image against $canary"
	# No network: decryption needs none, and the service's fixed MAC is in use.
	if ! docker run --rm --network none --entrypoint unfairplay \
		-v "$PWD/secrets/models:/secrets/models:ro" -v "$PWD/canary:/canary:ro" \
		"$image" verify --input "/canary/$(basename "$canary")" --model /secrets/models; then
		echo "canary failed; keeping the running version" >&2
		[[ -n "$old" ]] && docker tag "$old" "$image"
		exit 1
	fi
else
	echo "warning: no canary IPA in ./canary, skipping decryption check" >&2
fi

# Prefer restarting while idle; the bot also drains on SIGTERM.
source .env
for _ in $(seq 1 60); do
	status=$(curl -fsS -H "Authorization: Bearer $UNFAIRPLAY_BOT_API_TOKEN" http://127.0.0.1:8080/v1/status || true)
	if [[ -z "$status" ]]; then
		break # not running
	fi
	if grep -q '"running": 0,' <<<"$status" && grep -q '"queued": 0,' <<<"$status"; then
		break
	fi
	sleep 30
done
docker compose up -d bot
docker image prune -f >/dev/null
echo "updated to $new"
