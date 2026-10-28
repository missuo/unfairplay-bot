#!/usr/bin/env bash
# Moves ipatool (go.mod) and UnfairPlay (deps.env) to the newest version
# allowed by their tracking policy. Prints what changed; exits 0 either way.
set -euo pipefail
cd "$(dirname "$0")/.."

IPATOOL_MODULE=github.com/majd/ipatool/v2
UNFAIRPLAY_MODULE=github.com/DrWhax/unfairplay

# shellcheck disable=SC1091
source deps.env
export GOFLAGS=-mod=mod

resolve() { # module policy
	case "$2" in
	main) go list -m -f '{{.Version}}' "$1@main" ;;
	release) go list -m -f '{{.Version}}' "$1@latest" ;;
	prerelease) go list -m -versions "$1" | tr ' ' '\n' | tail -1 ;;
	*) echo "unknown policy '$2' for $1" >&2 && exit 1 ;;
	esac
}

changes=()

current_ipatool=$(go list -m -f '{{.Version}}' "$IPATOOL_MODULE")
next_ipatool=$(resolve "$IPATOOL_MODULE" "$IPATOOL_TRACK")
if [[ "$next_ipatool" != "$current_ipatool" ]]; then
	go get "$IPATOOL_MODULE@$next_ipatool"
	go mod tidy
	changes+=("ipatool $current_ipatool -> $next_ipatool")
fi

next_unfairplay=$(resolve "$UNFAIRPLAY_MODULE" "$UNFAIRPLAY_TRACK")
if [[ "$next_unfairplay" != "$UNFAIRPLAY_VERSION" ]]; then
	sed -i.bak "s/^UNFAIRPLAY_VERSION=.*/UNFAIRPLAY_VERSION=$next_unfairplay/" deps.env && rm deps.env.bak
	changes+=("unfairplay $UNFAIRPLAY_VERSION -> $next_unfairplay")
fi

if ((${#changes[@]} == 0)); then
	echo "dependencies are current"
else
	printf '%s\n' "${changes[@]}"
fi
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
	{
		echo "changed=$(((${#changes[@]} > 0)) && echo true || echo false)"
		echo "summary=$(IFS=', '; echo "${changes[*]:-}")"
	} >>"$GITHUB_OUTPUT"
fi
