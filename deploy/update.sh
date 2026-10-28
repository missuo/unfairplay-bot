#!/usr/bin/env bash
# Server-side updater, run by unfairplay-bot-update.timer from the compose
# directory. Pulls the newest image and switches to it only if it passes two
# canaries:
#   1. App Store: `unfairplay-bot check` with the existing ipatool session
#      (catches an ipatool update that broke login or the download API);
#   2. UnfairPlay: `unfairplay verify` on ./canary/*.ipa with the private model
#      (catches a decryption regression).
# A failed canary keeps the running version.
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

rollback() {
	echo "$1; keeping the running version" >&2
	[[ -n "$old" ]] && docker tag "$old" "$image"
	exit 1
}

source .env
status() {
	curl -fsS -H "Authorization: Bearer $UNFAIRPLAY_BOT_API_TOKEN" http://127.0.0.1:8080/v1/status 2>/dev/null || true
}

# 1. App Store canary. Join the running container's network namespace so the
# check uses the same MAC address (ipatool's device identity).
running=$(docker compose ps -q bot 2>/dev/null || true)
if [[ -n "$running" ]]; then
	network=(--network "container:$running")
else
	network=(--mac-address "$(docker compose config --format json | grep -o '"mac_address": *"[^"]*"' | head -1 | cut -d'"' -f4)")
fi
set +e
docker run --rm "${network[@]}" --env-file .env \
	-v "$PWD/ipatool:/home/bot/.ipatool" -v "$PWD/cache:/home/bot/.cache" \
	"$image" unfairplay-bot check
code=$?
set -e
case $code in
0) echo "App Store canary passed" ;;
2) echo "warning: UNFAIRPLAY_BOT_HEALTH_APP_ID not set, skipping App Store canary" >&2 ;;
*)
	# If the running version is already broken, a failing new version is no
	# worse; otherwise do not risk replacing a working one.
	if grep -q '"ok": false' <<<"$(status)"; then
		echo "App Store canary failed, but the running version is unhealthy too" >&2
	else
		rollback "App Store canary failed"
	fi
	;;
esac

# 2. UnfairPlay canary. No network: decryption is offline.
canary=$(find canary -maxdepth 1 -name '*.ipa' 2>/dev/null | head -1 || true)
if [[ -n "$canary" ]]; then
	docker run --rm --network none --entrypoint unfairplay \
		-v "$PWD/secrets/models:/secrets/models:ro" -v "$PWD/canary:/canary:ro" \
		"$image" verify --input "/canary/$(basename "$canary")" --model /secrets/models ||
		rollback "UnfairPlay canary failed"
	echo "UnfairPlay canary passed"
else
	echo "warning: no canary IPA in ./canary, skipping decryption check" >&2
fi

# Prefer restarting while idle (the bot also drains on SIGTERM). When the
# running version cannot reach the App Store, every task fails anyway, so
# switch at once.
for _ in $(seq 1 60); do
	s=$(status)
	if [[ -z "$s" ]] || grep -q '"ok": false' <<<"$s"; then
		break
	fi
	if grep -q '"running": 0,' <<<"$s" && grep -q '"queued": 0,' <<<"$s"; then
		break
	fi
	sleep 30
done
docker compose up -d bot
docker image prune -f >/dev/null
echo "updated to $new"
