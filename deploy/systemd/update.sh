#!/usr/bin/env bash
# Native (no Docker) build and rollout, run as root by
# unfairplay-bot-update.timer. Layout:
#   /opt/unfairplay-bot/src              git checkout of this repository
#   /opt/unfairplay-bot/releases/<sha>   built binaries + build.env
#   /opt/unfairplay-bot/current          symlink to the running release
#   /etc/unfairplay-bot/env              settings and secrets (root, 0600)
#   /var/lib/unfairplay-bot              service home: data, models, canary
#
# A new commit on main (including the CI's dependency bumps) is built, tested,
# and must pass two canaries before it replaces the running release:
#   1. App Store: `unfairplay-bot check` with the existing session;
#   2. UnfairPlay: `unfairplay verify` on canary/*.ipa with the private model.
set -euo pipefail

# Everything runs from main so bash has parsed the whole file before
# `git checkout` replaces it.
main() {
	ROOT=/opt/unfairplay-bot
	STATE=/var/lib/unfairplay-bot
	ENV_FILE=/etc/unfairplay-bot/env
	export PATH=/usr/local/go/bin:$PATH
	export GOPATH=/var/cache/unfairplay-bot/go GOCACHE=/var/cache/unfairplay-bot/go-build
	export GOFLAGS=-mod=readonly CGO_ENABLED=1

	cd "$ROOT/src"
	git fetch -q origin main
	sha=$(git rev-parse origin/main)
	current=none
	if [[ -L "$ROOT/current" ]]; then current=$(basename "$(readlink -f "$ROOT/current")"); fi
	if [[ "$sha" == "$current" && "${1:-}" != "--force" ]]; then
		echo "already running $sha"
		exit 0
	fi
	git checkout -q --detach "$sha"

	release=$ROOT/releases/$sha
	if [[ ! -x "$release/bin/unfairplay-bot" ]]; then
		echo "building $sha"
		go test ./...
		rm -rf "$release.tmp" && mkdir -p "$release.tmp/bin"
		go build -trimpath -o "$release.tmp/bin/unfairplay-bot" .
		ipatool=$(go list -m -f '{{.Version}}' github.com/majd/ipatool/v2)
		# shellcheck disable=SC1091
		. ./deps.env
		GOBIN=$release.tmp/bin GOFLAGS= go install -trimpath "github.com/majd/ipatool/v2@$ipatool"
		GOBIN=$release.tmp/bin GOFLAGS= CGO_ENABLED=0 go install -trimpath "github.com/DrWhax/unfairplay/cmd/unfairplay@$UNFAIRPLAY_VERSION"
		echo "UNFAIRPLAY_REF=$UNFAIRPLAY_VERSION" >"$release.tmp/build.env"
		chmod -R a+rX "$release.tmp"
		mv -T "$release.tmp" "$release"
	fi

	token=$(sed -n 's/^UNFAIRPLAY_BOT_API_TOKEN=//p' "$ENV_FILE")
	status() { curl -fsS -H "Authorization: Bearer $token" http://127.0.0.1:8080/v1/status 2>/dev/null || true; }
	as_service() { # run a command as the service user with the service's environment
		systemd-run --quiet --wait --pipe --collect --uid=unfairplay --gid=unfairplay \
			-p EnvironmentFile="$ENV_FILE" -p Environment=HOME="$STATE" \
			-p Environment=UNFAIRPLAY_BOT_DATA_DIR="$STATE/data" -p Environment=UNFAIRPLAY_BOT_MODEL="$STATE/models" \
			"$@"
	}

	if [[ "$current" != none ]]; then
		# 1. App Store canary (never signs in, so it cannot use up login attempts).
		set +e
		as_service "$release/bin/unfairplay-bot" check
		code=$?
		set -e
		case $code in
		0) echo "App Store canary passed" ;;
		2) echo "warning: UNFAIRPLAY_BOT_HEALTH_APP_ID not set, skipping App Store canary" >&2 ;;
		*)
			if grep -q '"ok": false' <<<"$(status)"; then
				echo "App Store canary failed, but the running release is unhealthy too" >&2
			else
				echo "App Store canary failed; keeping $current" >&2
				exit 1
			fi
			;;
		esac

		# 2. UnfairPlay canary (offline).
		canary=$(find "$STATE/canary" -maxdepth 1 -name '*.ipa' 2>/dev/null | head -1 || true)
		if [[ -n "$canary" ]]; then
			as_service "$release/bin/unfairplay" verify --input "$canary" --model "$STATE/models" >/dev/null ||
				{ echo "UnfairPlay canary failed; keeping $current" >&2; exit 1; }
			echo "UnfairPlay canary passed"
		else
			echo "warning: no canary IPA in $STATE/canary, skipping decryption check" >&2
		fi

		# Prefer switching while idle; switch at once if the running release is broken.
		for _ in $(seq 1 60); do
			s=$(status)
			if [[ -z "$s" ]] || grep -q '"ok": false' <<<"$s"; then break; fi
			if grep -q '"running": 0,' <<<"$s" && grep -q '"queued": 0,' <<<"$s"; then break; fi
			sleep 30
		done
	fi

	ln -sfn "$release" "$ROOT/current.tmp" && mv -T "$ROOT/current.tmp" "$ROOT/current"
	systemctl restart unfairplay-bot
	echo "running $sha"

	# Keep the three newest releases.
	ls -1dt "$ROOT"/releases/*/ | tail -n +4 | xargs -r rm -rf
}

main "$@"
exit
