package netdiag

import (
	"net"
	"strings"
	"testing"
)

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://172.20.15.226/snap.jpeg": "172.20.15.226",
		"http://camera.example.lan/x.jpg": "camera.example.lan",
		"https://unifi.lan:7443":          "unifi.lan",
		"":                                "",
		"not a url":                       "",
	}
	for input, want := range cases {
		if got := HostOf(input); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveAcceptsALiteralAddress(t *testing.T) {
	addrs, err := Resolve("172.20.15.226")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "172.20.15.226" {
		t.Errorf("Resolve returned %v", addrs)
	}
}

// The container always has at least one non-loopback interface, and reporting
// them is the whole point of the diagnosis.
func TestInterfacesReportsSubnets(t *testing.T) {
	interfaces := Interfaces()
	if len(interfaces) == 0 {
		t.Skip("no non-loopback interfaces in this environment")
	}
	for _, iface := range interfaces {
		if iface.Name == "" {
			t.Error("an interface was reported with no name")
		}
		for _, subnet := range iface.Subnets {
			if _, _, err := net.ParseCIDR(subnet); err != nil {
				t.Errorf("%s reported an unparseable subnet %q", iface.Name, subnet)
			}
		}
	}
}

// An address on one of the container's own subnets never leaves the container,
// so it must be reported; anything outside them must not be.
func TestConflictsMatchOnlyLocalSubnets(t *testing.T) {
	interfaces := Interfaces()
	if len(interfaces) == 0 || len(interfaces[0].Subnets) == 0 {
		t.Skip("no usable interface in this environment")
	}

	var local *net.IPNet
	for _, iface := range interfaces {
		for _, subnet := range iface.Subnets {
			ip, ipNet, err := net.ParseCIDR(subnet)
			if err != nil || ip.To4() == nil {
				continue
			}
			local = ipNet
			break
		}
		if local != nil {
			break
		}
	}
	if local == nil {
		t.Skip("no IPv4 interface in this environment")
	}

	// A neighbour of ours, which is exactly the failure case.
	neighbour := make(net.IP, len(local.IP.To4()))
	copy(neighbour, local.IP.To4())
	neighbour[3] ^= 0x20
	if !local.Contains(neighbour) {
		t.Skip("could not construct a neighbouring address")
	}

	conflicts := Conflicts(neighbour.String())
	if len(conflicts) == 0 {
		t.Errorf("%s is inside %s and should have been reported", neighbour, local)
	}
	for _, conflict := range conflicts {
		if !strings.Contains(conflict.String(), conflict.Subnet) {
			t.Errorf("the description should name the subnet: %s", conflict)
		}
	}

	// A documentation address will not be on any container subnet.
	if outside := Conflicts("203.0.113.7"); len(outside) != 0 {
		t.Errorf("203.0.113.7 should not conflict, got %v", outside)
	}
}

func TestConflictsToleratesAnUnresolvableHost(t *testing.T) {
	if got := Conflicts("this-host-does-not-exist.invalid"); got != nil {
		t.Errorf("expected no conflicts for an unresolvable host, got %v", got)
	}
}
