# syntax=docker/dockerfile:1

# --- build ------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first so the layer is cached; this project has none beyond the
# standard library, but the pattern keeps rebuilds cheap if that ever changes.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

# CGO is off so the result is a static binary that runs on a scratch base.
# The version is compiled in; the web frontend uses it to invalidate caches.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/timelapse ./cmd/timelapse

# --- runtime ----------------------------------------------------------------
# distroless/static provides CA certificates for HTTPS to the Protect console
# and nothing else: no shell, no package manager, minimal attack surface.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="unifi-protect-timelapse" \
      org.opencontainers.image.description="Timelapse capture service for UniFi Protect cameras with local buffering and archive sync" \
      org.opencontainers.image.source="https://github.com/steiner-dominik/unifi-protect-timelapse" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/timelapse /timelapse

# Defaults for the in-container paths; override with volumes in compose.
ENV SPOOL_DIR=/spool \
    ARCHIVE_DIR=/archive \
    STATE_DIR=/state \
    WEB_ADDR=:8080

EXPOSE 8080

# The binary checks its own persisted state, so this works even when the web
# interface is disabled. It reports unhealthy when captures have stopped inside
# the active window, which is the failure that previously went unnoticed.
HEALTHCHECK --interval=2m --timeout=10s --start-period=1m --retries=2 \
    CMD ["/timelapse", "healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/timelapse"]
CMD ["serve"]
