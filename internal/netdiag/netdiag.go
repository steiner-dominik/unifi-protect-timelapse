// Package netdiag answers a question containers make surprisingly hard: can
// this process actually reach that address, and if not, is its own network in
// the way?
//
// Docker allocates bridge networks from 172.17.0.0/12 by default, which is the
// same private range many people use for their LAN. When a camera's address
// falls inside a subnet the container is itself attached to, the container
// treats it as a neighbour on the bridge and never routes it to the LAN. The
// symptom is a camera that answers from the host and times out from inside.
package netdiag

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Interface is one of the container's own network interfaces.
type Interface struct {
	Name    string   `json:"name"`
	Subnets []string `json:"subnets"`
}

// Conflict reports that a target address falls inside one of the container's
// own subnets, so traffic to it never leaves the container's network.
type Conflict struct {
	Host      string `json:"host"`
	IP        string `json:"ip"`
	Interface string `json:"interface"`
	Subnet    string `json:"subnet"`
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s (%s) is inside this container's own subnet %s on %s",
		c.Host, c.IP, c.Subnet, c.Interface)
}

// Interfaces lists the container's interfaces and their subnets, loopback
// excluded.
func Interfaces() []Interface {
	found, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var result []Interface
	for _, iface := range found {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		entry := Interface{Name: iface.Name}
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				entry.Subnets = append(entry.Subnets, ipNet.String())
			}
		}
		if len(entry.Subnets) > 0 {
			result = append(result, entry)
		}
	}
	return result
}

// HostOf extracts the hostname from a URL.
func HostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// Resolve returns the addresses a host resolves to.
func Resolve(host string) ([]string, error) {
	if host == "" {
		return nil, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return []string{ip.String()}, nil
	}

	addrs, err := net.LookupHost(host)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", host, err)
	}
	return addrs, nil
}

// Conflicts reports every way the container's own networking shadows the given
// host. An empty result means the address is not on any of the container's
// subnets, so it will be routed out normally.
func Conflicts(host string) []Conflict {
	addresses, err := Resolve(host)
	if err != nil {
		return nil
	}

	var conflicts []Conflict
	for _, address := range addresses {
		ip := net.ParseIP(address)
		if ip == nil {
			continue
		}

		for _, iface := range Interfaces() {
			for _, subnet := range iface.Subnets {
				_, ipNet, err := net.ParseCIDR(subnet)
				if err != nil || !ipNet.Contains(ip) {
					continue
				}
				// The container's own address is not a conflict, it is just
				// itself.
				if strings.HasPrefix(subnet, ip.String()+"/") {
					continue
				}
				conflicts = append(conflicts, Conflict{
					Host:      host,
					IP:        ip.String(),
					Interface: iface.Name,
					Subnet:    ipNet.String(),
				})
			}
		}
	}
	return conflicts
}

// DefaultGateway reads the container's default route. Linux only; anywhere else
// it simply reports nothing rather than guessing.
func DefaultGateway() string {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Scan() // header

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		gateway, err := hex.DecodeString(fields[2])
		if err != nil || len(gateway) != 4 {
			continue
		}
		// /proc/net/route stores the address little-endian.
		ip := make(net.IP, 4)
		binary.LittleEndian.PutUint32(ip, binary.BigEndian.Uint32(gateway))
		return fmt.Sprintf("%s via %s", ip, fields[0])
	}
	return ""
}
