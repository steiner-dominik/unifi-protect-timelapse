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

# CGO is off so the binary is static and depends on nothing in the runtime
# image. The version is compiled in; the web frontend uses it to invalidate
# cached assets on every release.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/timelapse ./cmd/timelapse

# --- runtime ----------------------------------------------------------------
# Alpine rather than distroless because timelapse video rendering needs ffmpeg.
# Everything else the binary needs is compiled in, so the image stays small:
# ca-certificates for HTTPS to the Protect console, and nothing further.
FROM alpine:3.22

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="unifi-protect-timelapse" \
      org.opencontainers.image.description="Timelapse capture service for UniFi Protect cameras with local buffering, archive sync and video rendering" \
      org.opencontainers.image.source="https://github.com/steiner-dominik/unifi-protect-timelapse" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

RUN apk add --no-cache ca-certificates ffmpeg tzdata \
 && addgroup -g 1000 -S timelapse \
 && adduser -u 1000 -S -G timelapse timelapse \
 && mkdir -p /spool /archive /state \
 && chown timelapse:timelapse /spool /archive /state

COPY --from=build /out/timelapse /usr/local/bin/timelapse

# Defaults for the in-container paths; override with volumes in compose.
ENV SPOOL_DIR=/spool \
    ARCHIVE_DIR=/archive \
    STATE_DIR=/state \
    WEB_ADDR=:8080

EXPOSE 8080

# The binary checks its own persisted state, so this works even when the web
# interface is disabled. It reports unhealthy when captures stall inside the
# active window, or, for a watchdog deployment, when the camera stops answering
# or the archive stops growing.
HEALTHCHECK --interval=2m --timeout=15s --start-period=1m --retries=2 \
    CMD ["/usr/local/bin/timelapse", "healthcheck"]

USER timelapse:timelapse
ENTRYPOINT ["/usr/local/bin/timelapse"]
CMD ["serve"]
