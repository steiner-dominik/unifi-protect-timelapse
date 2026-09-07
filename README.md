# unifi-protect-timelapse

Captures still images from a UniFi Protect camera on a schedule, buffers them on
local disk, and moves them to an archive (typically an NFS share) when that
archive is reachable. Ships a web interface for watching it, browsing the
archive and rendering timelapse videos.

It is a container replacement for a Raspberry Pi that did the same job with two
cron jobs and a pair of shell scripts. The buffering behaviour is the reason the
design looks the way it does: **if the archive is unavailable, images accumulate
locally and are transferred once it comes back** — a NAS rebooting for an hour
costs you nothing.

- Two snapshot sources — the camera's anonymous endpoint and the UniFi Protect
  integration API — with automatic fallback between them
- Capturing never depends on the NAS being reachable, and Docker mounts the NFS
  share itself, so nothing needs to be in the host's fstab
- A sentinel-file check that refuses to write into an unmounted mountpoint
- Catches silent failures: stalled captures, an unreachable camera, a frozen
  camera returning identical frames, and gaps in the archive
- Web UI with light/dark themes, English and German, installable as a PWA
- Runs as a Home Assistant add-on, with or without capturing
- Health check, Prometheus metrics, Home Assistant entities and a failure webhook
- Zero external Go dependencies

---

## Quick start

```bash
curl -LO https://github.com/steiner-dominik/unifi-protect-timelapse/releases/latest/download/compose.yaml
curl -L -o .env https://github.com/steiner-dominik/unifi-protect-timelapse/releases/latest/download/env.example
$EDITOR .env
docker compose up -d
```

Then open `http://<host>:8099`.

One thing must exist on the NAS share before the first run:

```bash
touch /path/to/the/share/.nas
```

That sentinel is not optional. See [The sentinel file](#the-sentinel-file).

---

## How it works

Two containers, because capturing must never depend on the NAS:

```
   ┌── timelapse ────────────────────┐        ┌── archive ──────────────────┐
   │  captures every interval        │        │  holds the NFS mount        │
   │  writes to the local spool      │───────▶│  moves spooled images onto  │
   │  serves the web UI              │ spool  │  it when the sentinel is    │
   │  NO archive mount               │◀───────│  present                    │
   └─────────────────────────────────┘  API   └─────────────────────────────┘
              always starts                        restarts until the NAS
                                                   comes back — harmlessly
```

The capturing container has no archive mount at all, so Docker can never fail to
start it. The archive container holds the NFS volume; if the NAS is unreachable
Docker cannot mount it and that container restarts until it can, which is
harmless because the images are safe in the spool meanwhile. The web UI asks the
archive container for anything archive related, so you still get one dashboard
and one timelapse viewer.

Images are stored in the layout the Raspberry Pi produced, so an existing archive
stays uniform:

```
<root>/YYYY/YYYY-MM/YYYY-MM-DD/<prefix>YYYY-MM-DD-HH-MM-SS.jpg
```

### The sentinel file

Before moving anything, the service checks that `ARCHIVE_SENTINEL` (default
`.nas`) exists inside `ARCHIVE_DIR`. If it is missing, the sync aborts and the
images stay in the spool.

This matters because **an unmounted NFS share is indistinguishable from an empty
local directory**. Without the check, a failed mount would mean the service
happily fills the container's own filesystem while deleting the originals. The
file lives on the NAS, so it is only visible when the share is really mounted.

### Sync modes

| Mode | Behaviour |
| --- | --- |
| `opportunistic` (default) | Move each image right after capture. Combined with `SYNC_INTERVAL` in the split deployment, where the capture trigger lives elsewhere. |
| `nightly` | Only the nightly sweep at `SYNC_AT`, exactly like the original cron job. |
| `off` | Never move anything. This is what the capturing container uses, since the archive container does the moving. |

`SYNC_INTERVAL` adds a sweep on a fixed cadence, and the nightly sweep always
runs as the catch-up path for anything buffered during an outage.

---

## Getting a frame from the camera

### Anonymous snapshot (`snapshot`)

The camera's own endpoint, the method the Pi used. Requires *Anonymous Snapshot*
to be enabled for the camera in UniFi Protect.

```bash
CAMERA_SOURCE=snapshot
CAMERA_SNAPSHOT_URL=http://camera.example.lan/snap.jpeg
```

### Protect integration API (`protect`)

Goes through the UniFi OS console with an API key, and can return a
higher-resolution frame. Create the key under **Settings → Control Plane →
Integrations**.

```bash
CAMERA_SOURCE=protect
PROTECT_HOST=https://unifi.example.lan
PROTECT_API_KEY=...
PROTECT_CAMERA_ID=...
```

To find the camera ID:

```bash
docker compose run --rm timelapse cameras
```

### Fallback

`CAMERA_FALLBACK_SOURCE` names a second source to try whenever the primary
fails, so you can prefer the Protect API and fall back to the camera directly:

```bash
CAMERA_SOURCE=protect
CAMERA_FALLBACK_SOURCE=snapshot
```

The primary is retried on **every** capture, so recovery is automatic and needs
no cooldown. The status page and the `timelapse_camera_fallback_active` metric
show which source is actually serving frames, so a silent degradation to the
lesser source is visible rather than invisible.

---

## Catching silent failures

The setup this replaces failed silently for weeks: the camera's IP address
changed, captures stopped, and nothing said so. Four checks now cover that class
of problem, and each one feeds the health check, the metrics and the UI.

| Check | What it catches |
| --- | --- |
| Capture freshness | Captures have stopped inside the active window |
| Camera probe | The camera does not answer at all |
| Frozen frames | The camera answers, with the same bytes, forever |
| Archive freshness | Images are being captured but not landing in the archive |
| Gap detection | Stretches of a past day where images are missing |

Frozen-frame detection compares each frame with its predecessor;
`FROZEN_FRAME_THRESHOLD` identical frames in a row is treated as a dead camera.
Gap detection derives gaps from the spacing of the frames that are actually
there, rather than from an expected schedule, because the interval and the
window have changed over the years and a historical day should not be judged
against today's settings.

---

## Watchdog mode

Set `CAPTURE_ENABLED=false` and the service captures nothing. With
`MONITOR_ENABLED` (which defaults on exactly there) it instead probes the camera
and watches how fresh the archive is — a health dashboard that cannot disturb
the real capturer. The live view falls back to the newest archived image, so
there is still something to look at.

This is what the Home Assistant add-on runs.

---

## Home Assistant

The same image runs as an add-on from
[home-assistant-apps](https://github.com/steiner-dominik/home-assistant-apps),
with capturing and syncing switched off. Point it at the share through Home
Assistant's own network storage (**Settings → System → Storage**) and it gives
you the live frame, the archive and a health check inside Home Assistant.

The UI runs behind ingress, so Home Assistant authenticates every request and
`WEB_AUTH_MODE=ingress` skips the app's own token.

### Entities

When the Supervisor token is present the service writes entities straight to the
Core API — no MQTT broker, no template sensors:

| Entity | Meaning |
| --- | --- |
| `binary_sensor.timelapse_camera_online` | The camera answered the last probe |
| `binary_sensor.timelapse_archive_available` | The sentinel is present |
| `binary_sensor.timelapse_frame_frozen` | Identical frames are being returned |
| `sensor.timelapse_archive_newest` | Timestamp of the newest archived image |
| `sensor.timelapse_spool_files` | Images currently buffered locally |
| `sensor.timelapse_status` | `ok`, `failing`, `camera_offline`, `frozen` or `buffering` |

`HA_ENTITY_PREFIX` renames them; `HA_PUBLISH_ENABLED=false` switches them off.

---

## Configuration

Everything is read from the environment; there are no config files and no
compiled-in defaults that reference a particular deployment. Invalid values are
reported **all at once** at startup, so a misconfigured deploy takes one restart
to fix rather than five.

### Paths

| Variable | Default | Description |
| --- | --- | --- |
| `SPOOL_DIR` | `/spool` | Local buffer. Keep on fast local storage. |
| `ARCHIVE_DIR` | `/archive` | Where images end up. |
| `STATE_DIR` | `/state` | Bookkeeping and the latest-image mirror. |
| `TZ` | `UTC` | Folder names use local time — set this explicitly. |

### Camera

| Variable | Default | Description |
| --- | --- | --- |
| `CAMERA_SOURCE` | `snapshot` | `snapshot` or `protect`. |
| `CAMERA_FALLBACK_SOURCE` | — | Second source, tried when the primary fails. |
| `CAMERA_SNAPSHOT_URL` | — | Required for the `snapshot` source. |
| `PROTECT_HOST` | — | Required for `protect`. Include the scheme. |
| `PROTECT_API_KEY` | — | Required for `protect`. |
| `PROTECT_CAMERA_ID` | — | Required for `protect`. |
| `PROTECT_HIGH_QUALITY` | `true` | Request the full-resolution frame. |
| `PROTECT_INSECURE_TLS` | `false` | Skip certificate verification. |
| `CAMERA_TIMEOUT` | `20s` | Per-attempt timeout. |
| `CAMERA_RETRIES` | `3` | Attempts before a source counts as failed. |
| `CAMERA_RETRY_DELAY` | `3s` | Delay between attempts. |
| `MIN_IMAGE_BYTES` | `1024` | Anything smaller is rejected. |

### Capture and schedule

| Variable | Default | Description |
| --- | --- | --- |
| `CAPTURE_ENABLED` | `true` | |
| `CAPTURE_INTERVAL` | `5m` | Ticks are aligned to the wall clock. |
| `FILENAME_PREFIX` | `snapshot_` | Prepended to the timestamp. |
| `SCHEDULE_MODE` | `fixed` | `fixed` or `solar`. |
| `ACTIVE_HOURS` | `05-21` | Inclusive end hour, matching cron's `5-21`. |
| `LATITUDE` / `LONGITUDE` | — | Required for `solar`. |
| `SOLAR_DAWN_OFFSET` | `-30m` | Start this much before sunrise. |
| `SOLAR_DUSK_OFFSET` | `30m` | Stop this much after sunset. |

### Sync

| Variable | Default | Description |
| --- | --- | --- |
| `SYNC_MODE` | `opportunistic` | `opportunistic`, `nightly` or `off`. |
| `SYNC_INTERVAL` | — | Additional sweep on a fixed cadence. |
| `SYNC_AT` | `23:00` | Time of the nightly sweep. |
| `ARCHIVE_SENTINEL` | `.nas` | The mount guard. |
| `SYNC_PRUNE_EMPTY_DIRS` | `true` | Never touches the current day. |

### Monitoring

| Variable | Default | Description |
| --- | --- | --- |
| `MONITOR_ENABLED` | on when not capturing | Camera probe and archive freshness. |
| `MONITOR_INTERVAL` | `5m` | How often to probe. |
| `ARCHIVE_MAX_AGE` | 3× capture interval | Staleness limit for the archive. |
| `FROZEN_FRAME_THRESHOLD` | `3` | Identical frames meaning a dead camera. `0` disables. |
| `METRICS_ENABLED` | `true` | Exposes `/metrics`. |
| `NOTIFY_WEBHOOK_URL` | — | POST target for failure notifications. |
| `NOTIFY_FAILURE_THRESHOLD` | `3` | Consecutive failures before notifying. |
| `HA_PUBLISH_ENABLED` | on with a Supervisor token | Publish Home Assistant entities. |
| `HA_ENTITY_PREFIX` | `timelapse` | Entity id prefix. |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `text` | `text` or `json`. |

### Web interface

| Variable | Default | Description |
| --- | --- | --- |
| `WEB_ENABLED` | `true` | |
| `WEB_ADDR` | `:8080` | Inside the container; compose publishes 8099. |
| `WEB_AUTH_MODE` | derived | `none`, `local` or `ingress`. |
| `WEB_AUTH_TOKEN` | — | Required for `local`. |
| `ARCHIVE_PROXY_URL` | — | Delegate archive reads to another instance. |
| `ARCHIVE_PROXY_TOKEN` | — | Token for that instance, if it uses `local`. |
| `WEB_DEFAULT_LANGUAGE` | `en` | Used when the browser has no match. |
| `WEB_I18N_DIR` | — | Serve translations from disk instead. |
| `WEB_SITE_NAME` | — | Optional subtitle in the header. |
| `WEB_ARCHIVE_ENABLED` | `true` | Enable the archive browser. |
| `WEB_ZIP_ENABLED` | `true` | Enable ZIP download. |
| `WEB_LIVE_PREVIEW` | `true` | Enable the live preview button. |
| `WEB_LIVE_MIN_INTERVAL` | `2s` | Rate limit for live previews. |

### Video

| Variable | Default | Description |
| --- | --- | --- |
| `VIDEO_ENABLED` | `true` | Render timelapse videos with ffmpeg. |
| `VIDEO_FPS` | `12` | Default frame rate. |
| `VIDEO_CRF` | `23` | Quality; 0 is lossless, 51 is worst. |
| `VIDEO_MAX_FRAMES` | `5000` | Refuse renders larger than this. |
| `VIDEO_TIMEOUT` | `10m` | Per-render limit. |

---

## Web interface

Three tabs:

- **Live** — the last archived frame, plus a *live preview* button that fetches a
  fresh frame from the camera and **never writes it to disk**. The archive keeps
  exactly one image per interval, so timelapse spacing stays uniform.
- **Timelapse** — browse by year, month and day, play a day back with a scrubber
  and adjustable speed, see where the gaps are, and download the day as an MP4 or
  a ZIP. Days containing several capture series can be filtered by series so
  playback does not jump between camera framings.
- **Status** — the active window, next capture, last success and its age, failure
  counters, camera reachability, archive freshness, how much is buffered, and a
  read-only view of the running configuration. **Secrets are never included**;
  the API key appears only as a yes/no.

The UI is plain HTML, CSS and JavaScript — no framework, no bundler, no npm. It
is installable as a PWA and works offline for the shell.

### Authentication

`WEB_AUTH_MODE` selects the behaviour. It defaults to `local` when
`WEB_AUTH_TOKEN` is set and `none` otherwise. In `local` mode, open the UI once
as:

```
http://host:8099/?token=<your-token>
```

The token is exchanged for a `HttpOnly`, `SameSite=Strict` cookie and stripped
from the URL, so it does not linger in your address bar or history. Scripted
access can use `Authorization: Bearer <token>`. `/healthz` and `/metrics` stay
reachable without credentials so the container runtime and your monitoring can
use them.

### Adding a language

Translations are plain JSON files. Copy
[`internal/web/assets/i18n/en.json`](internal/web/assets/i18n/en.json), translate
the values, and either drop it in that directory and rebuild, or mount a
directory with the files and point `WEB_I18N_DIR` at it — no rebuild needed. A
test asserts every language defines exactly the same keys.

---

## Monitoring

### Health check

The container reports **unhealthy** when, inside the active window, captures
have stalled, the camera has frozen, the camera does not answer, or the archive
has stopped growing. Outside the window and during startup it stays healthy.

```bash
docker inspect --format '{{.State.Health.Status}}' timelapse
```

### Metrics

`GET /metrics` in Prometheus text format, including
`timelapse_captures_total{result}`, `timelapse_last_capture_success_timestamp_seconds`,
`timelapse_spool_files`, `timelapse_archive_available`,
`timelapse_camera_online`, `timelapse_camera_fallback_active`,
`timelapse_frame_frozen` and `timelapse_archive_newest_timestamp_seconds`.

A rising `timelapse_spool_files` with `timelapse_archive_available` at 0 is the
signature of a NAS outage — a normal, handled condition, not an emergency.

### Failure webhook

`NOTIFY_WEBHOOK_URL` receives a JSON POST on repeated capture failures, a frozen
camera, a failed sync, or an archive still unavailable at the nightly sweep.
Repeats of the same kind are suppressed for an hour.

---

## Commands

```bash
docker compose run --rm timelapse capture-once   # capture one frame and exit
docker compose run --rm timelapse sync-once      # run one sync pass and exit
docker compose run --rm timelapse cameras        # list Protect camera IDs
docker compose run --rm timelapse healthcheck    # exit non-zero if unhealthy
docker compose run --rm timelapse version
```

---

## Migrating from the Raspberry Pi

1. Set `FILENAME_PREFIX` to whatever the Pi used, so the archive stays uniform.
2. Point the share at a **staging directory** and run both for a day. Compare
   filenames and sizes against what the Pi produced.
3. Switch to the real path and remove the Pi's crontab entries.
4. Copy anything left on the Pi's USB drive to the NAS:
   ```bash
   rsync -rtlh --remove-source-files /media/usb0/timelapse/ /mnt/nas/timelapse/
   ```
5. Decommission the Pi.

Worth knowing about the scripts this replaces: `timelapse.sh` wrote to
`/media/usb0/timelapse/` while `move_to_nas.sh` read from `/media/usb/timelapse/`
— check both paths before wiping the drive.

---

## Releases

Versions are `YY.MM.NN`: the year, the month, and a sequence within that month.
Tagging `v26.09.01` publishes `ghcr.io/steiner-dominik/unifi-protect-timelapse`
as `26.09.01` and `latest` for `linux/amd64`, `linux/arm64` and `linux/arm/v7`,
with build provenance, and creates a GitHub release carrying that version's
[CHANGELOG](CHANGELOG.md) section plus a matching `compose.yaml` and
`env.example`. Pushes to `main` publish `:edge`.

The Home Assistant add-on pins the exact version, so every release here needs a
matching add-on bump — publish this release first.

---

## Development

```bash
go test ./...              # run the tests
go run ./cmd/timelapse     # run locally (reads the same environment variables)
go run ./tools/genicons    # regenerate the PWA icons
```

The icons are generated from code rather than committed as opaque binaries; CI
verifies they still match their generator. Local builds report version `dev`,
which deliberately disables long-lived asset caching so edited CSS and
JavaScript take effect on reload.

---

## Security notes

- `compose.yaml` runs both services as user `1000:1000` with all capabilities
  dropped and `no-new-privileges`. Neither container needs any elevated
  privilege, and neither mounts anything on the host. The image itself does not
  pin a user, because a Home Assistant add-on has to run as root to read the
  configuration the Supervisor writes for it — so pass `--user` if you run it
  with `docker run` rather than compose.
- Nothing is hard-coded: all configuration comes from the environment.
- The debug view is built from an explicitly redacted projection of the
  configuration. The API key is reduced to a boolean and URLs are stripped of
  userinfo and query strings. A test asserts no secret reaches the status
  response.
- Every path component from a URL is validated against a strict pattern, and the
  resolved path is checked to be inside its root.
- The ingress prefix is echoed into the page, so it is sanitised to a plain
  absolute path rather than trusted verbatim.
- A strict Content-Security-Policy (`default-src 'none'`, no `unsafe-inline`) is
  possible because the frontend uses no inline scripts or styles and loads
  nothing from a third-party origin.
- Snapshot responses are validated as JPEGs by status, content type, size and
  magic bytes, so an HTTP error page is never archived as an image.

Found a security problem? Please open an issue, or contact me via
[dominik.st/einer](https://dominik.st/einer).

---

## Disclaimer

This is an independent community project. It is **not affiliated with, endorsed
by, or sponsored by** Ubiquiti Inc., Raspberry Pi Ltd., Docker Inc., GitHub Inc.,
the Home Assistant project, Nabu Casa Inc., or any other company or product
referenced here. UniFi and UniFi Protect are trademarks of Ubiquiti Inc.; all
other trademarks are the property of their respective owners. Names are used
only to describe interoperability.

Provided as is, without warranty of any kind. Use at your own risk.

## License

[MIT](LICENSE) — a project by [dominik.st/einer](https://dominik.st/einer).
