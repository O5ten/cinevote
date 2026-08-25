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

# ---- runtime ----------------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -H -u 10001 cinevote \
 && mkdir -p /data && chown cinevote:cinevote /data

COPY --from=build /out/cinevote /usr/local/bin/cinevote

USER cinevote
WORKDIR /data

ENV CINEVOTE_ADDR=:8080 \
    CINEVOTE_DB=/data/cinevote.db

EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/cinevote"]
