# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
FROM golang:1.22-alpine AS build

WORKDIR /src

# Dependencies first, so a code-only change keeps the module cache warm.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# Pure-Go SQLite (modernc.org/sqlite) means CGO stays off and the binary is
# static: nothing to link against in the final image.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/cinevote ./cmd/cinevote

# ---- base runtime -----------------------------------------------------------
# Shared by both images. Two targets follow: "production", which keeps its data,
# and "demo", which throws it away and reseeds on every start.
FROM alpine:3.20 AS runtime

RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -H -u 10001 cinevote \
 && mkdir -p /data && chown cinevote:cinevote /data

COPY --from=build /out/cinevote /usr/local/bin/cinevote

USER cinevote
# The working directory is where a mounted .env is picked up from, and where a
# relative CINEVOTE_DB would land.
WORKDIR /data

ENV CINEVOTE_ADDR=:8080

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/cinevote"]

# ---- production -------------------------------------------------------------
# The image to run for a real movie night. Mount a volume on /data to keep the
# database, and mount a file on /data/.env to configure it that way instead of
# with -e flags.
FROM runtime AS production

ENV CINEVOTE_DB=/data/cinevote.db
VOLUME ["/data"]
CMD []

# ---- demo -------------------------------------------------------------------
# Runs itself: accounts, films and votes are seeded at startup into a throwaway
# database, so there is nothing to configure and no volume to keep.
#
#   docker run --rm -p 8080:8080 ghcr.io/o5ten/cinevote-demo
FROM runtime AS demo

ENV CINEVOTE_DEMO=true
CMD ["-demo"]
