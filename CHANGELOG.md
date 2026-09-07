# Changelog

All notable changes to this project are documented here.

Versions are `YY.MM.NN`: the year, the month, and a sequence within that month.
The Home Assistant add-on pins the exact version, so every release here must
have a matching add-on bump in
[home-assistant-apps](https://github.com/steiner-dominik/home-assistant-apps).

## 26.09.02

### Added

- **Camera fallback.** `CAMERA_FALLBACK_SOURCE` names a second source to try
  when the primary fails, so the Protect API can be preferred with the camera's
  anonymous snapshot endpoint behind it. The primary is retried on every
  capture, so recovery needs no intervention. The status page and
  `timelapse_camera_fallback_active` show which source is actually in use.
- **Watchdog mode.** `MONITOR_ENABLED` probes the camera and checks how fresh
  the archive is without writing anything. It defaults on wherever capturing is
  off, which is what makes a non-capturing deployment useful as a health check.
- **Frozen-camera detection.** A camera can answer with byte-identical frames
  forever while looking healthy by every other measure.
  `FROZEN_FRAME_THRESHOLD` consecutive identical frames now mark the service
  unhealthy and fire a notification.
- **Gap detection.** Each day reports where images are missing, derived from the
  spacing of the frames that are there rather than from today's schedule, so
  historical days are judged fairly.
- **Timelapse video rendering.** A day can be rendered to MP4 on demand, with a
  selectable frame rate.
- **ZIP download.** A day can be downloaded as a ZIP of the original JPEGs.
- **Home Assistant entities.** Running as an add-on, the service writes
  `binary_sensor.timelapse_camera_online`, `sensor.timelapse_archive_newest`,
  `sensor.timelapse_spool_files` and others straight to the Core API using the
  Supervisor token. No broker and no template sensors.
- **Home Assistant ingress.** The frontend now works behind a generated
  sub-path, and `WEB_AUTH_MODE=ingress` defers authentication to Home Assistant.
- **Periodic sync.** `SYNC_INTERVAL` sweeps the buffer on a fixed cadence, for
  the split deployment where capture happens in a different container.
- **Archive delegation.** `ARCHIVE_PROXY_URL` points an instance without an
  archive mount at one that has it, for both browsing and status.

### Changed

- **Two containers by default.** The capturing container no longer mounts the
  archive at all, so it always starts and keeps capturing while the NAS is
  unreachable. A second container holds the NFS mount, which Docker now performs
  itself: nothing needs to be in the host's fstab.
- The default published port is 8099 rather than 8080.
- The runtime image is Alpine rather than distroless, because video rendering
  needs ffmpeg.
- `WEB_AUTH_MODE` replaces the implicit "a token means auth is on" behaviour,
  which remains the default when the mode is unset.

## 26.09.01

### Added

- First release. Captures stills from a UniFi Protect camera on a schedule,
  buffers them locally, and moves them to an archive when it is reachable.
  Replaces a Raspberry Pi running two cron jobs.
- Anonymous snapshot and UniFi Protect integration API sources.
- Fixed-hours or sunrise/sunset capture windows.
- Sentinel-file guard so an unmounted share is never written into.
- Web interface with live view, archive browser, status page, English and
  German, light and dark, installable as a PWA.
- Health check, Prometheus metrics and a failure webhook.
