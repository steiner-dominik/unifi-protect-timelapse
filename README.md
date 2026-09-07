# unifi-protect-timelapse

Captures still images from a UniFi Protect camera on a schedule, buffers them on
local disk, and moves them to an archive (typically an NFS share) when that
archive is reachable. Ships a small web interface for checking on it.

It is a container replacement for a Raspberry Pi that did the same job with two
cron jobs and a pair of shell scripts. The buffering behaviour is the reason the
design looks the way it does: **if the archive is unavailable, images accumulate
locally and are transferred once it comes back** — a NAS rebooting for an hour
costs you nothing.

- Single static binary, no runtime dependencies beyond the Go standard library
- Two ways to get a frame: the camera's anonymous snapshot endpoint, or the
  UniFi Protect integration API
- Atomic writes, so a sync can never pick up a half-written image
- A sentinel-file check that refuses to write into an unmounted mountpoint
- Health check, Prometheus metrics and an optional failure webhook
- Web UI with light/dark themes, English and German, installable as a PWA

---

## Quick start

```bash
git clone https://github.com/steiner-dominik/unifi-protect-timelapse.git
cd unifi-protect-timelapse
cp .env.example .env
$EDITOR .env
docker compose up -d
```

Then open `http://<host>:8080`.

Before the first run, two things need to exist on the host:

```bash
# 1. The NFS share, mounted as usual via /etc/fstab
sudo mount /mnt/nas/timelapse

# 2. The sentinel file, on the share itself
touch /mnt/nas/timelapse/.nas
```

That second step is not optional. See [The sentinel file](#the-sentinel-file).

---

## How it works

```
                 every CAPTURE_INTERVAL, inside the active window
                                    |
   camera  ──HTTP──▶  validate  ──▶ write atomically ──▶  SPOOL_DIR
                     (JPEG? size?)                          │
                                                            │ when the archive
                                                            │ is reachable
                                                            ▼
                                        sentinel present? ──▶ ARCHIVE_DIR
                                                  │
                                                  └── no ──▶ stay in the spool,
                                                             retry tonight
```

Images are stored in the layout the Raspberry Pi produced, so an existing
archive stays uniform:

```
<root>/YYYY/YYYY-MM/YYYY-MM-DD/<prefix>YYYY-MM-DD-HH-MM-SS.jpg
```

### Sync modes

| Mode | Behaviour |
| --- | --- |
| `opportunistic` (default) | Move each image right after capture, plus a nightly sweep to catch up. At most one interval of images sits on local disk. |
| `nightly` | Only the nightly sweep at `SYNC_AT`, exactly like the original cron job. |
| `off` | Never move anything; images stay in `SPOOL_DIR`. |

### The sentinel file

Before moving anything, the service checks that `ARCHIVE_SENTINEL` (default
`.nas`) exists inside `ARCHIVE_DIR`. If it is missing, the sync aborts and the
images stay in the spool.

This matters because **an unmounted NFS share is indistinguishable from an empty
local directory**. Without the check, a failed mount would mean the service
happily fills the container's own filesystem while deleting the originals. The
file lives on the NAS, so it is only visible when the share is actually mounted.

---

## Getting a frame from the camera

### Anonymous snapshot (`CAMERA_SOURCE=snapshot`)

The camera's own endpoint, the method the Pi used. Requires *Anonymous Snapshot*
to be enabled for the camera in UniFi Protect.

```bash
CAMERA_SOURCE=snapshot
CAMERA_SNAPSHOT_URL=http://camera.example.lan/snap.jpeg
```

### Protect integration API (`CAMERA_SOURCE=protect`)

Goes through the UniFi OS console with an API key, and can return a
higher-resolution frame. Create the key under **Settings → Control Plane →
Integrations**.

```bash
CAMERA_SOURCE=protect
PROTECT_HOST=https://unifi.example.lan
PROTECT_API_KEY=...
PROTECT_CAMERA_ID=...
PROTECT_HIGH_QUALITY=true
```

To find the camera ID:

```bash
docker compose run --rm timelapse cameras
```

If the console presents a self-signed certificate, set
`PROTECT_INSECURE_TLS=true`. It is off by default and logs a warning when
enabled.

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
| `ARCHIVE_DIR` | `/archive` | Where images end up. The mounted share. |
| `STATE_DIR` | `/state` | Bookkeeping and the latest-image mirror. |
| `TZ` | `UTC` | Folder names use local time — set this explicitly. |

### Camera

| Variable | Default | Description |
| --- | --- | --- |
| `CAMERA_SOURCE` | `snapshot` | `snapshot` or `protect`. |
| `CAMERA_SNAPSHOT_URL` | — | Required for `snapshot`. |
| `PROTECT_HOST` | — | Required for `protect`. Include the scheme. |
| `PROTECT_API_KEY` | — | Required for `protect`. |
| `PROTECT_CAMERA_ID` | — | Required for `protect`. |
| `PROTECT_HIGH_QUALITY` | `true` | Request the full-resolution frame. |
| `PROTECT_INSECURE_TLS` | `false` | Skip certificate verification. |
| `CAMERA_TIMEOUT` | `20s` | Per-attempt timeout. |
| `CAMERA_RETRIES` | `3` | Attempts before a capture counts as failed. |
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
| `SYNC_AT` | `23:00` | Time of the nightly sweep. |
| `ARCHIVE_SENTINEL` | `.nas` | The mount guard. |
| `SYNC_PRUNE_EMPTY_DIRS` | `true` | Never touches the current day. |

### Web interface

| Variable | Default | Description |
| --- | --- | --- |
| `WEB_ENABLED` | `true` | |
| `WEB_ADDR` | `:8080` | |
| `WEB_AUTH_TOKEN` | — | Empty means no authentication. |
| `WEB_DEFAULT_LANGUAGE` | `en` | Used when the browser has no match. |
| `WEB_I18N_DIR` | — | Serve translations from disk instead. |
| `WEB_SITE_NAME` | — | Optional subtitle in the header. |
| `WEB_ARCHIVE_ENABLED` | `true` | Enable the archive browser. |
| `WEB_LIVE_PREVIEW` | `true` | Enable the live preview button. |
| `WEB_LIVE_MIN_INTERVAL` | `2s` | Rate limit for live previews. |

### Monitoring

| Variable | Default | Description |
| --- | --- | --- |
| `METRICS_ENABLED` | `true` | Exposes `/metrics`. |
| `NOTIFY_WEBHOOK_URL` | — | POST target for failure notifications. |
| `NOTIFY_FAILURE_THRESHOLD` | `3` | Consecutive failures before notifying. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `LOG_FORMAT` | `text` | `text` or `json`. |

---

## Web interface

Three tabs:

- **Live** — the last archived frame, plus a *live preview* button that fetches a
  fresh frame from the camera and **never writes it to disk**. The archive keeps
  exactly one image per interval, so timelapse spacing stays uniform.
- **Timelapse** — browse the archive by year, month and day and play a day back
  with a scrubber and adjustable speed. Days that contain images from several
  capture series (an older camera and a newer one, say) can be filtered by
  series so playback does not jump between framings.
- **Status** — everything you would want when something is wrong: the active
  window, next capture, last success and its age, failure counters, the last
  error, whether the archive is mounted, and how much is buffered locally. Plus
  a read-only view of the running configuration. **Secrets are never included**;
  the API key appears only as a yes/no.

The UI is plain HTML, CSS and JavaScript — no framework, no bundler, no npm. It
is installable as a PWA and works offline for the shell (images and status are
always fetched fresh, since a cached snapshot would be misleading).

### Authentication

Off by default, which is appropriate on a trusted LAN. Setting `WEB_AUTH_TOKEN`
turns it on. Open the UI once as:

```
http://host:8080/?token=<your-token>
```

The token is exchanged for a `HttpOnly`, `SameSite=Strict` cookie and stripped
from the URL, so it does not linger in your address bar or history. Scripted
access can use `Authorization: Bearer <token>` instead. `/healthz` and
`/metrics` stay reachable without credentials so the container runtime and your
monitoring can use them.

### Adding a language

Translations are plain JSON files. Copy
[`internal/web/assets/i18n/en.json`](internal/web/assets/i18n/en.json), translate
the values, and either drop it in that directory and rebuild, or mount a
directory with the files and point `WEB_I18N_DIR` at it — no rebuild needed. The
language picker is populated from whatever files it finds.

---

## Monitoring

### Health check

The container reports **unhealthy** when the last successful capture is older
than twice the capture interval, while inside the active window. Outside the
window and during startup it stays healthy.

This exists because the setup this replaces failed silently: the camera's IP
address changed, captures stopped, and nothing said so for weeks.

```bash
docker inspect --format '{{.State.Health.Status}}' timelapse
```

### Metrics

`GET /metrics` in Prometheus text format:

| Metric | Meaning |
| --- | --- |
| `timelapse_captures_total{result}` | Capture attempts by outcome |
| `timelapse_capture_consecutive_failures` | Failures since the last success |
| `timelapse_last_capture_success_timestamp_seconds` | For alerting on staleness |
| `timelapse_sync_runs_total{result}` | Sync runs by outcome |
| `timelapse_spool_files` / `timelapse_spool_bytes` | How much is buffered locally |
| `timelapse_archive_available` | 1 when the sentinel is present |

A rising `timelapse_spool_files` with `timelapse_archive_available` at 0 is the
signature of a NAS outage — which is a normal, handled condition, not an
emergency.

### Failure webhook

Set `NOTIFY_WEBHOOK_URL` to receive a JSON POST on repeated capture failures, a
failed sync, or an archive that is still unavailable at the nightly sweep.
Repeats of the same kind are suppressed for an hour.

```json
{
  "service": "unifi-protect-timelapse",
  "kind": "capture_failed",
  "severity": "error",
  "title": "Timelapse capture is failing",
  "message": "The last 3 capture attempts failed. Most recent error: ...",
  "timestamp": "2026-09-07T13:45:00+02:00"
}
```

---

## Commands

The image's entrypoint takes a subcommand:

```bash
docker compose run --rm timelapse capture-once   # capture one frame and exit
docker compose run --rm timelapse sync-once      # run one sync pass and exit
docker compose run --rm timelapse cameras        # list Protect camera IDs
docker compose run --rm timelapse healthcheck    # exit non-zero if stalled
docker compose run --rm timelapse version
```

---

## NFS and permissions

The compose file bind-mounts a path the host has already mounted. That is
deliberate: the directory then always exists, so **the container starts even
when the NAS is down**, the sentinel check does its job, and images buffer
locally. A Docker-managed NFS volume fails the mount instead and the container
never starts — which would stop capturing as well. The alternative is included
as a commented block in [`compose.yaml`](compose.yaml) if you want it anyway.

NFS maps permissions by numeric ID, so `PUID`/`PGID` have to match the owner of
the files on the share:

```bash
stat -c '%u:%g' /mnt/nas/timelapse
```

---

## Migrating from the Raspberry Pi

1. Set `FILENAME_PREFIX` to whatever the Pi used, so the archive stays uniform.
2. Point `ARCHIVE_PATH` at a **staging directory** on the NAS and run both for a
   day. Compare filenames and sizes against what the Pi produced.
3. Switch `ARCHIVE_PATH` to the real path and remove the Pi's crontab entries.
4. Copy anything left on the Pi's USB drive to the NAS:
   ```bash
   rsync -rtlh --remove-source-files /media/usb0/timelapse/ /mnt/nas/timelapse/
   ```
5. Decommission the Pi.

Worth knowing about the scripts this replaces: `timelapse.sh` wrote to
`/media/usb0/timelapse/` while `move_to_nas.sh` read from `/media/usb/timelapse/`
— check both paths before wiping the drive.

---

## Development

```bash
go test ./...              # run the tests
go run ./cmd/timelapse     # run locally (reads the same environment variables)
go run ./tools/genicons    # regenerate the PWA icons
```

The icons are generated from code rather than committed as opaque binaries; CI
verifies they still match their generator.

Releases: pushing to `main` publishes `ghcr.io/steiner-dominik/unifi-protect-timelapse:edge`.
Pushing a `v1.2.3` tag publishes `:1.2.3`, `:1.2`, `:1` and `:latest`, for
`linux/amd64`, `linux/arm64` and `linux/arm/v7`, with build provenance
attestation.

---

## Security notes

- Runs as a non-root user in a distroless image with no shell, all capabilities
  dropped, `no-new-privileges` and a read-only root filesystem.
- Nothing is hard-coded: all configuration comes from the environment.
- The debug view is built from an explicitly redacted projection of the
  configuration. The API key is reduced to a boolean and URLs are stripped of
  userinfo and query strings before rendering. A test asserts that no secret
  reaches the status response.
- Every path component from a URL is validated against a strict pattern, and the
  resolved path is checked to be inside its root.
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
or any other company or product referenced here. UniFi and UniFi Protect are
trademarks of Ubiquiti Inc.; all other trademarks are the property of their
respective owners. Names are used only to describe interoperability.

Provided as is, without warranty of any kind. Use at your own risk.

## License

[MIT](LICENSE) — a project by [dominik.st/einer](https://dominik.st/einer).
