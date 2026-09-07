package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/camera"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/monitor"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/netdiag"
)

// runDiagnose prints everything needed to work out why the camera or the
// archive is not reachable from inside this container, and then actually tries
// to fetch a frame.
//
// It exists because the two failures people hit are invisible from outside: an
// archive path that is not mounted, and a camera address that falls inside the
// container's own Docker subnet, which makes it unreachable from in here while
// answering perfectly from the host.
func runDiagnose() error {
	cfg, log, _, err := setup()
	if err != nil {
		return err
	}
	_ = log

	public := cfg.Public()

	section("Service")
	fmt.Printf("  version           %s\n", version)
	fmt.Printf("  timezone          %s\n", public.Timezone)
	fmt.Printf("  capture           %v\n", cfg.Capture.Enabled)
	fmt.Printf("  monitor           %v\n", cfg.Monitor.Enabled)
	fmt.Printf("  sync mode         %s\n", public.SyncMode)

	section("Camera")
	fmt.Printf("  source            %s\n", public.CameraSource)
	if public.CameraFallback != "" {
		fmt.Printf("  fallback          %s\n", public.CameraFallback)
	}
	fmt.Printf("  endpoint          %s\n", public.CameraTarget)
	fmt.Printf("  skip TLS checks   %v\n", cfg.Camera.InsecureTLS)

	hosts := cameraHosts(cfg)
	for _, host := range hosts {
		addresses, err := netdiag.Resolve(host)
		if err != nil {
			fmt.Printf("  %-17s RESOLUTION FAILED: %v\n", host, err)
			continue
		}
		fmt.Printf("  %-17s resolves to %v\n", host, addresses)
	}

	section("This container's network")
	interfaces := netdiag.Interfaces()
	if len(interfaces) == 0 {
		fmt.Println("  (no interfaces reported)")
	}
	for _, iface := range interfaces {
		fmt.Printf("  %-17s %v\n", iface.Name, iface.Subnets)
	}
	if gateway := netdiag.DefaultGateway(); gateway != "" {
		fmt.Printf("  %-17s %s\n", "default route", gateway)
	}

	section("Address overlap")
	var conflicts []netdiag.Conflict
	for _, host := range hosts {
		conflicts = append(conflicts, netdiag.Conflicts(host)...)
	}
	if len(conflicts) == 0 {
		fmt.Println("  none: the camera is outside this container's own subnets,")
		fmt.Println("  so traffic to it is routed out normally.")
	} else {
		for _, conflict := range conflicts {
			fmt.Printf("  PROBLEM: %s\n", conflict)
		}
		fmt.Println()
		fmt.Println("  The container treats that address as a neighbour on its own")
		fmt.Println("  bridge and never sends it to the LAN. It will answer from the")
		fmt.Println("  host and time out from in here. Move Docker off that range, for")
		fmt.Println("  example in /etc/docker/daemon.json:")
		fmt.Println()
		fmt.Println(`    { "default-address-pools":`)
		fmt.Println(`      [ { "base": "10.201.0.0/16", "size": 24 } ] }`)
	}

	section("Archive")
	fmt.Printf("  directory         %s\n", public.ArchiveDir)
	archiveState, detail := monitor.Inspect(cfg)
	fmt.Printf("  state             %s\n", archiveState)
	if detail != "" {
		fmt.Printf("  detail            %s\n", detail)
	}
	if path, at, err := monitor.NewestFrame(cfg); err == nil {
		fmt.Printf("  newest image      %s (%s)\n", path, at.Format(time.RFC3339))
	}

	section("Live fetch")
	if err := tryFetch(cfg); err != nil {
		fmt.Printf("  FAILED: %v\n", err)
		fmt.Println()
		fmt.Println("  If the address overlap above is clean and this still fails,")
		fmt.Println("  the camera is refusing or unreachable for another reason:")
		fmt.Println("  check the URL, the port, and whether the snapshot endpoint")
		fmt.Println("  is enabled on the camera.")
		os.Exit(1)
	}
	return nil
}

// tryFetch performs a real snapshot request and reports how it went.
func tryFetch(cfg *config.Config) error {
	source, err := camera.Build(cfg, discardingLogger())
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		cfg.Camera.Timeout*time.Duration(max(cfg.Camera.Retries, 1))+10*time.Second)
	defer cancel()

	started := time.Now()
	data, err := source.Snapshot(ctx)
	elapsed := time.Since(started).Truncate(time.Millisecond)
	if err != nil {
		return fmt.Errorf("after %s: %w", elapsed, err)
	}

	fmt.Printf("  OK: %d bytes in %s via the %s source\n",
		len(data), elapsed, camera.LastUsed(source))
	return nil
}

// cameraHosts returns the hostnames the configuration actually uses.
func cameraHosts(cfg *config.Config) []string {
	seen := make(map[string]struct{})
	var hosts []string

	for _, raw := range []string{cfg.Camera.SnapshotURL, cfg.Camera.ProtectHost} {
		host := netdiag.HostOf(raw)
		if host == "" {
			continue
		}
		if _, duplicate := seen[host]; duplicate {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	return hosts
}

func section(title string) {
	fmt.Printf("\n%s\n%s\n", title, dashes(len(title)))
}

func dashes(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = '-'
	}
	return string(out)
}
