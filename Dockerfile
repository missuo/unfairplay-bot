# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# ipatool links the keyring with cgo, so build natively (no cross-compiling).
RUN CGO_ENABLED=1 go build -trimpath -o /out/unfairplay-bot . \
 && GOBIN=/out CGO_ENABLED=1 go install -trimpath \
      "github.com/majd/ipatool/v2@$(go list -m -f '{{.Version}}' github.com/majd/ipatool/v2)" \
 && . ./deps.env \
 && GOBIN=/out CGO_ENABLED=0 go install -trimpath \
      "github.com/DrWhax/unfairplay/cmd/unfairplay@${UNFAIRPLAY_VERSION}" \
 && echo "UNFAIRPLAY_REF=${UNFAIRPLAY_VERSION}" > /out/build.env

# ipatool downloads glibc (manylinux) emulator libraries at runtime: keep Debian.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --create-home --home-dir /home/bot bot \
 && install -d -o bot -g bot -m 700 /data /home/bot/.ipatool /home/bot/.cache
COPY --from=build /out/unfairplay-bot /out/ipatool /out/unfairplay /usr/local/bin/
COPY --from=build /out/build.env /etc/unfairplay-bot.env
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
USER bot
ENV HOME=/home/bot \
    UNFAIRPLAY_BOT_LISTEN=0.0.0.0:8080 \
    UNFAIRPLAY_BOT_DATA_DIR=/data \
    UNFAIRPLAY_BOT_MODEL=/secrets/models
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["unfairplay-bot"]
