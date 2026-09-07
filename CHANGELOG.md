# Changelog

All notable changes to this project are documented here.

Versions are `YY.MM.NN`: the year, the month, and a sequence within that month.
The Home Assistant add-on pins the exact version, so every release here needs a
matching add-on bump in
[home-assistant-apps](https://github.com/steiner-dominik/home-assistant-apps) —
publish the release here first.

## 26.09.01

First release. Replaces a Raspberry Pi that captured a frame every five minutes
with two cron jobs and a pair of shell scripts.

### Capture and archive

- Captures stills from a UniFi Protect camera on a schedule and buffers them on
  local disk, moving them to an archive when it is reachable. If the archive is
  unavailable the images accumulate locally and transfer once it returns.
- Two snapshot sources: the camera's anonymous endpoint, and the UniFi Protect
  integration API with an API key. `CAMERA_FALLBACK_SOURCE` names a second
  source to try whenever the primary fails; the primary is retried on every
  capture, so recovery needs no intervention.
- Keeps the `YYYY/YYYY-MM/YYYY-MM-DD/<prefix>...jpg` layout of the setup it
  replaces, so an existing archive stays uniform.
- A sentinel file must be present in the archive before anything is moved. An
  unmounted NFS share is otherwise indistinguishable from an empty local
  directory, and the sync would fill the local filesystem while deleting the
  originals.
- Images are written to a temporary name and renamed, so a sync can never pick
  up a half-written frame.
- Capture windows are either fixed hours or derived from sunrise and sunset,
  computed with the standard library.

### Catching silent failures

The setup this replaces stopped capturing for weeks after the camera's IP
address changed, and nothing reported it. Five checks now cover that class of
problem, each feeding the health check, the metrics and the UI:

- Snapshots are validated by status, content type, size and JPEG magic bytes, so
  an HTTP error page is never archived as an image.
- Capture freshness: captures have stopped inside the active window.
- A camera probe: the camera does not answer at all.
- Frozen-frame detection: the camera answers with byte-identical frames forever,
  which looks healthy by every other measure.
- Archive freshness and gap detection: images are captured but not landing, or a
  past day has missing stretches.

### Deployment

- Two containers. The capturing one has no archive mount, so Docker can never
  fail to start it; the other holds the NFS volume, which Docker mounts itself,
  so nothing needs to be in the host's fstab.
- `ARCHIVE_PROXY_URL` lets the container without the mount serve archive
  browsing and status from the one that has it, keeping a single dashboard.
- Runs as a non-root user with all capabilities dropped and no elevated
  privileges in either container.
- Published for `linux/amd64`, `linux/arm64` and `linux/arm/v7` with build
  provenance.

### Web interface

- Live view, archive browser with playback and gap reporting, and a status page
  built from an explicitly redacted projection of the configuration.
- A day can be rendered to MP4 or downloaded as a ZIP of the original JPEGs.
- Light and dark themes, English and German, installable as a PWA. No framework,
  no bundler, no npm.
- Optional token authentication, off by default.

### Home Assistant

- The same image runs as an add-on with capturing and syncing switched off,
  acting as a watchdog: it probes the camera and watches the archive without
  writing anything.
- The frontend works behind an ingress sub-path, and `WEB_AUTH_MODE=ingress`
  defers authentication to Home Assistant.
- Entities are written straight to the Core API using the Supervisor token — no
  broker and no template sensors.

### Monitoring

- Docker health check, Prometheus metrics, and a generic failure webhook.
