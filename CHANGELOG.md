# Changelog

All notable changes to this project are documented here.

Versions are `YY.MM.NN`: the year, the month, and a sequence within that month.
The Home Assistant add-on pins the exact version, so every release here needs a
matching add-on bump in
[home-assistant-apps](https://github.com/steiner-dominik/home-assistant-apps) —
publish the release here first.

## 26.09.05

### Fixed

- **The live preview always failed with "camera unreachable", whatever the
  camera was doing.** Adding ingress support in 26.09.02 introduced a
  module-level `url()` helper, and the preview function already had a local
  variable of the same name. A local declaration shadows the outer name for the
  whole function, so the helper was in its temporal dead zone and threw before
  the request was ever made. The `catch` turned that into a camera error. The
  server was answering correctly the entire time, which is why nothing appeared
  in the log. A test now fails if any function shadows the helper again.

- **A missing image showed a broken image icon.** The live view now fetches the
  image rather than assigning it to `src`, and renders an explanation instead:
  whether the archive directory is missing, unreadable, or simply has no images
  yet.

### Added

- **The archive is diagnosed rather than left blank.** Its state is checked at
  startup and on every probe and reported as `ok`, `missing`, `unreadable` or
  `empty`, with the path and what to check. It appears in the log, in
  `/api/status`, and on both the Live and Status tabs. An empty panel used to be
  indistinguishable from a wrong path.

- **Frontend failures are reported to the service log.** Anyone running this in
  a container has the log and nothing else, and a browser console they never
  open is not a diagnostic. The page now posts uncaught errors, unhandled
  rejections and handled failures to `/api/client-error`, which logs them. The
  endpoint is rate limited and size capped, and only ever writes to the log.

- The live preview endpoint now includes the underlying error in its response,
  and logs when it serves a frame.

### Note

An archive problem is deliberately **not** unhealthy. It is loud in the log and
in the interface, but it does not restart the container, because a health check
that restarts a service over a configuration mistake produces exactly the silent
restart loop fixed in 26.09.04.

## 26.09.04

### Fixed

- **The insecure TLS toggle did nothing for the anonymous snapshot source.** It
  was only ever applied when building the Protect source, so an HTTPS snapshot
  URL with a self-signed certificate — the normal case for a camera on a local
  address — failed however it was configured. It now applies to every camera
  request, and to both halves of a fallback chain. The setting is now
  `CAMERA_INSECURE_TLS`; `PROTECT_INSECURE_TLS` is still accepted.

- **The service restarted in a loop with nothing in its log.** Three separate
  faults combined here:

  - The `healthcheck` subcommand behind the container HEALTHCHECK had its own
    copy of the health logic, stricter than `/healthz`: no startup grace, and an
    archive with no images treated as a failure. The endpoint reported healthy
    while the health check failed and the runtime restarted the container. Both
    now call one shared implementation, and the subcommand reads the service's
    start time from the state file so it applies the same grace.
  - The reason was invisible. A health check runs as its own process, so its
    output goes to the container runtime rather than the service log. The
    service now logs every change in its own health verdict, and notifies.
  - `ARCHIVE_MAX_AGE` defaulted to three capture intervals even for an instance
    that captures nothing. A watchdog cannot know how often something else fills
    the archive, and against a 15 minute limit an archive written by a nightly
    bulk transfer looks broken all day. The default is now 26 hours when not
    capturing.

- **The newest-image scan gave up on the first unrelated directory.** It
  committed to the lexically greatest subdirectory at each level, so a NAS
  directory such as `@eaDir`, `#recycle` or `.snapshot` — several of which sort
  above a four digit year — made a full archive report as empty. It now matches
  only date directories and backtracks past empty ones, so an empty directory
  for today no longer hides yesterday's images.

- An archive that has never held an image is no longer unhealthy. It may simply
  be empty or newly mounted; only an archive that was growing and then stopped
  is worth restarting for.

### Changed

- The Home Assistant add-on defaults to the anonymous snapshot source with no
  fallback, so a fresh install works once the snapshot URL is filled in.
  Defaulting to the Protect source meant a new install refused to start until
  every Protect field was set.

## 26.09.03

### Fixed

- The Home Assistant add-on failed to start with
  `reading /data/options.json: permission denied`. The image declared
  `USER timelapse`, but the Supervisor writes an add-on's configuration as root
  with mode 0600 and does not change the container's user, so the service could
  not read its own configuration. Add-ons are expected to run as root, so the
  image no longer drops privileges.

  The standalone deployment is unaffected: `compose.yaml` now pins user
  `1000:1000` for both services, which is also what NFS needs to map ownership.
  Anyone running `docker run` by hand should pass `--user` to match.

- The error message for an unreadable options file now says what to do about it.

### Changed

- CI runs the add-on smoke test against a named volume with root-owned,
  mode 0600 options, which is what the Supervisor actually produces. The
  previous bind-mount version could not reproduce the failure, because Docker
  Desktop's file sharing ignores ownership.
- CI additionally smoke-tests the standalone deployment as a non-root user.

## 26.09.02

Fixes found while packaging the Home Assistant add-on. All three shared a root
cause: code that assumed an instance which does not capture also has nothing to
say about the archive.

### Fixed

- The image set `SPOOL_DIR`, `ARCHIVE_DIR` and `STATE_DIR` as Docker `ENV`
  defaults, which are indistinguishable from values an operator set and so
  silently overrode the add-on's own options. The add-on ignored its configured
  archive path entirely, and its state did not survive a restart. The binary
  already defaults to the same values, so the `ENV` block was removed.
- The archive browser only searched the archive when this instance was syncing
  to it, so a watchdog deployment could not browse the archive it exists to
  watch.
- The status page derived its own health verdict from capture state, reporting
  a perfectly healthy watchdog as needing attention. The server's verdict is now
  published in `/api/status` and used directly, so the UI, the container health
  check and the Home Assistant watchdog cannot disagree.

### Changed

- The status page hides capture-specific rows on an instance that does not
  capture, and names its role instead.
- The live view captions the newest archived image when there is no capture of
  its own to describe.
- CI runs the tests in a shuffled order, and smoke-tests the add-on options
  path against the built image.

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
