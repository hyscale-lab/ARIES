package node

import (
	"bufio"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/setup/internal/configs"
)

func parse(t *testing.T, release, goarch string) (Host, error) {
	t.Helper()
	return ParseOSRelease(bufio.NewScanner(strings.NewReader(release)), goarch)
}

func TestParseOSReleaseUbuntu(t *testing.T) {
	host, err := parse(t, `NAME="Ubuntu"
VERSION_ID="22.04"
ID=ubuntu
ID_LIKE=debian
VERSION_CODENAME=jammy
`, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if host.Pkg != "apt" || host.Codename != "jammy" || host.DebArch != "amd64" || host.Version != "22.04" {
		t.Errorf("host = %+v", host)
	}
}

// Derivatives are recognised through ID_LIKE, not only ID.
func TestParseOSReleaseRHELDerivative(t *testing.T) {
	host, err := parse(t, "ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"9.3\"\n", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if host.Pkg != "dnf" || host.DebArch != "arm64" {
		t.Errorf("host = %+v", host)
	}
}

func TestParseOSReleaseRejectsUnsupported(t *testing.T) {
	if _, err := parse(t, "ID=alpine\n", "amd64"); err == nil {
		t.Error("alpine should be rejected")
	}
	if _, err := parse(t, "ID=ubuntu\n", "riscv64"); err == nil {
		t.Error("riscv64 should be rejected")
	}
}

// Calico peers over BGP on 179; flannel's VXLAN backend needs UDP 8472. A
// firewall opened without them leaves pods on different nodes unable to talk.
func TestFirewallPortsFollowTheCNI(t *testing.T) {
	tcp, udp := FirewallPorts(RoleWorker, configs.CNICalico)
	if !slices.Contains(tcp, "179") || len(udp) != 0 {
		t.Errorf("calico worker: tcp %v udp %v", tcp, udp)
	}
	tcp, udp = FirewallPorts(RoleControlPlane, configs.CNIFlannel)
	if !slices.Contains(tcp, "6443") || !slices.Equal(udp, []string{"8472"}) {
		t.Errorf("flannel control plane: tcp %v udp %v", tcp, udp)
	}
	if tcp, _ := FirewallPorts(RoleWorker, configs.CNINone); slices.Contains(tcp, "6443") {
		t.Error("workers must not open the API server port")
	}
}
