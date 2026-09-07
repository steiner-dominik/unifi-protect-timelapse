// Command timelapse captures still images from a UniFi Protect camera on a
// schedule, buffers them on local disk, and moves them to an archive such as an
// NFS share when that archive is available.
//
// It replaces a Raspberry Pi running two cron jobs. The buffering behaviour is
// the point: if the archive is unreachable the images accumulate locally and
// are transferred once it comes back.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embeds the timezone database so TZ works in a scratch container image.
	_ "time/tzdata"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/camera"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/capture"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/notify"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/schedule"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/syncer"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...". It also
// drives cache invalidation in the web frontend, so every release serves fresh
// assets.
var version = "dev"

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	var err error
	switch command {
	case "serve":
		err = run()
	case "healthcheck":
		err = runHealthcheck()
	case "cameras":
		err = runListCameras()
	case "capture-once":
		err = runCaptureOnce()
	case "sync-once":
		err = runSyncOnce()
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", command)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `unifi-protect-timelapse `+version+`

Usage: timelapse [command]

Commands:
  serve          Run the capture scheduler, the sync scheduler and the web UI (default)
  capture-once   Capture a single frame and exit
  sync-once      Run one sync pass and exit
  cameras        List the cameras visible through the Protect integration API
  healthcheck    Exit non-zero when captures have stopped (used by Docker HEALTHCHECK)
  version        Print the version and exit

All configuration is read from the environment; see README.md.
`)
}

// setup performs the initialisation every command shares.
func setup() (*config.Config, *slog.Logger, *state.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid configuration:\n%w", err)
	}

	log := newLogger(cfg)

	store, err := state.Open(cfg.StateDir)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, log, store, nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func newSource(cfg *config.Config, log *slog.Logger) (camera.Source, error) {
	source, err := camera.New(cfg, log)
	if err != nil {
		return nil, err
	}
	return &camera.Retrying{
		Source:   source,
		Attempts: cfg.Camera.Retries,
		Delay:    cfg.Camera.RetryDelay,
		Log:      log,
	}, nil
}

func run() error {
	cfg, log, store, err := setup()
	if err != nil {
		return err
	}

	source, err := newSource(cfg, log)
	if err != nil {
		return err
	}

	scheduler := schedule.New(cfg)
	capturer := capture.New(cfg, source, store, log)
	sync := syncer.New(cfg, store, log)
	notifier := notify.New(cfg.Notify, log)

	log.Info("starting unifi-protect-timelapse",
		"version", version,
		"camera", source.Describe(),
		"timezone", cfg.Location.String(),
		"interval", cfg.Capture.Interval,
		"schedule", string(cfg.Schedule.Mode),
		"window", cfg.Public().ActiveWindow,
		"spool", cfg.Capture.SpoolDir,
		"archive", cfg.Public().ArchiveDir,
		"syncMode", string(cfg.Sync.Mode))

	if err := os.MkdirAll(cfg.Capture.SpoolDir, 0o750); err != nil {
		return fmt.Errorf("creating spool directory: %w", err)
	}

	if cfg.SyncEnabled() && !sync.ArchiveAvailable() {
		log.Warn("archive is not available at startup; images will be buffered locally until it returns",
			"archive", cfg.Sync.ArchiveDir, "sentinel", cfg.Sync.Sentinel)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runner := &runner{
		cfg:       cfg,
		log:       log,
		store:     store,
		capturer:  capturer,
		syncer:    sync,
		scheduler: scheduler,
		notifier:  notifier,
	}

	errCh := make(chan error, 3)
	started := 0

	if cfg.Capture.Enabled {
		started++
		go func() { errCh <- runner.captureLoop(ctx) }()
	} else {
		log.Warn("capture is disabled (CAPTURE_ENABLED=false)")
	}

	if cfg.SyncEnabled() {
		started++
		go func() { errCh <- runner.syncLoop(ctx) }()
	}

	if cfg.Web.Enabled {
		server, err := web.New(web.Options{
			Config:    cfg,
			State:     store,
			Capturer:  capturer,
			Syncer:    sync,
			Scheduler: scheduler,
			Log:       log,
			Version:   version,
		})
		if err != nil {
			return fmt.Errorf("preparing web interface: %w", err)
		}
		started++
		go func() { errCh <- server.Run(ctx) }()
	}

	if started == 0 {
		return errors.New("nothing to do: capture, sync and the web interface are all disabled")
	}

	// The first error wins; the shared context then unwinds the rest.
	err = <-errCh
	stop()
	for range started - 1 {
		select {
		case <-errCh:
		case <-time.After(15 * time.Second):
		}
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

func runCaptureOnce() error {
	cfg, log, store, err := setup()
	if err != nil {
		return err
	}
	source, err := newSource(cfg, log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		cfg.Camera.Timeout*time.Duration(cfg.Camera.Retries+1))
	defer cancel()

	result, err := capture.New(cfg, source, store, log).Capture(ctx)
	if err != nil {
		return err
	}
	log.Info("captured", "path", result.Path, "bytes", result.Bytes)
	return nil
}

func runSyncOnce() error {
	cfg, log, store, err := setup()
	if err != nil {
		return err
	}
	if !cfg.SyncEnabled() {
		return errors.New("sync is disabled (SYNC_MODE=off)")
	}

	report, err := syncer.New(cfg, store, log).Run(context.Background())
	if err != nil {
		return err
	}
	log.Info("sync complete", "files", report.Files, "bytes", report.Bytes, "duration", report.Duration)
	return nil
}

func runListCameras() error {
	cfg, _, _, err := setup()
	if err != nil {
		return err
	}
	if cfg.Camera.ProtectHost == "" || cfg.Camera.ProtectAPIKey == "" {
		return errors.New("PROTECT_HOST and PROTECT_API_KEY must be set to list cameras")
	}

	client := &http.Client{Timeout: cfg.Camera.Timeout}
	if cfg.Camera.ProtectInsecureTLS {
		client.Transport = insecureTransport()
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Camera.Timeout)
	defer cancel()

	cameras, err := camera.ListCameras(ctx, client, cfg.Camera.ProtectHost, cfg.Camera.ProtectAPIKey)
	if err != nil {
		return err
	}
	if len(cameras) == 0 {
		fmt.Println("no cameras returned")
		return nil
	}

	fmt.Printf("%-26s  %-22s  %s\n", "ID", "NAME", "STATE")
	for _, cam := range cameras {
		fmt.Printf("%-26s  %-22s  %s\n", cam.ID, cam.Name, cam.State)
	}
	fmt.Println("\nSet PROTECT_CAMERA_ID to the ID of the camera you want to capture.")
	return nil
}

// runHealthcheck backs the container health check. It reads the persisted state
// rather than talking to the HTTP server, so it also works when the web
// interface is disabled.
func runHealthcheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Capture.Enabled {
		return nil
	}

	data, err := state.Load(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("no state written yet: %w", err)
	}

	if !schedule.New(cfg).Active(time.Now().In(cfg.Location)) {
		return nil
	}
	if data.LastCaptureSuccess.IsZero() {
		return errors.New("no successful capture recorded")
	}
	if age := time.Since(data.LastCaptureSuccess); age > 2*cfg.Capture.Interval {
		return fmt.Errorf("last successful capture was %s ago", age.Truncate(time.Second))
	}
	return nil
}
