#!/bin/sh
# Exposes the UnfairPlay version baked into the image to /v1/status.
set -a
. /etc/unfairplay-bot.env
set +a
exec "$@"
