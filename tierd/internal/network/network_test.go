package network

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Interface validation ---

func TestValidateInterfaceName(t *testing.T) {
	valid := []string{"eth0", "enp3s0", "bond0", "vlan.100", "ens192"}
	for _, name := range valid {
		if err := ValidateInterfaceName(name); err != nil {
			t.Errorf("expected valid interface name %q, got error: %v", name, err)
		}
	}

	invalid := []string{"", "0eth", "-bad", "has space", "a-name-that-is-way-too-long-for-an-interface"}
	for _, name := range invalid {
		if err := ValidateInterfaceName(name); err == nil {
			t.Errorf("expected invalid interface name %q to fail", name)
		}
	}
}

func TestValidateIPv4CIDR(t *testing.T) {
	valid := []string{"192.168.1.50/24", "10.0.0.1/8", "172.16.0.1/16"}
	for _, addr := range valid {
		if err := ValidateIPv4CIDR(addr); err != nil {
			t.Errorf("expected valid IPv4 CIDR %q, got error: %v", addr, err)
		}
	}

	invalid := []string{"192.168.1.50", "not-an-ip/24", "", "192.168.1.0"}
	for _, addr := range invalid {
		if err := ValidateIPv4CIDR(addr); err == nil {
			t.Errorf("expected invalid IPv4 CIDR %q to fail", addr)
		}
	}
}

func TestValidateIPv6CIDR(t *testing.T) {
	valid := []string{"2001:db8::1/64", "fd00::50/64", "::1/128"}
	for _, addr := range valid {
		if err := ValidateIPv6CIDR(addr); err != nil {
			t.Errorf("expected valid IPv6 CIDR %q, got error: %v", addr, err)
		}
	}

	invalid := []string{"192.168.1.0/24", "not-ipv6", "2001:db8::1"}
	for _, addr := range invalid {
		if err := ValidateIPv6CIDR(addr); err == nil {
			t.Errorf("expected invalid IPv6 CIDR %q to fail", addr)
		}
	}
}

func TestValidateIPv4(t *testing.T) {
	valid := []string{"192.168.1.1", "10.0.0.1", "8.8.8.8"}
	for _, addr := range valid {
		if err := ValidateIPv4(addr); err != nil {
			t.Errorf("expected valid IPv4 %q, got error: %v", addr, err)
		}
	}

	invalid := []string{"192.168.1.0/24", "not-an-ip", "", "2001:db8::1"}
	for _, addr := range invalid {
		if err := ValidateIPv4(addr); err == nil {
			t.Errorf("expected invalid IPv4 %q to fail", addr)
		}
	}
}

func TestValidateMTU(t *testing.T) {
	valid := []int{576, 1500, 9000}
	for _, mtu := range valid {
		if err := ValidateMTU(mtu); err != nil {
			t.Errorf("expected valid MTU %d, got error: %v", mtu, err)
		}
	}

	invalid := []int{0, 100, 575, 9001, 65535}
	for _, mtu := range invalid {
		if err := ValidateMTU(mtu); err == nil {
			t.Errorf("expected invalid MTU %d to fail", mtu)
		}
	}
}

// --- Network file generation ---

func TestGenerateNetworkFileStatic(t *testing.T) {
	cfg := InterfaceConfig{
		Name:      "eth0",
		IPv4Addrs: []string{"192.168.1.50/24"},
		Gateway4:  "192.168.1.1",
		IPv6Addrs: []string{"fd00::50/64"},
		Gateway6:  "fd00::1",
		MTU:       1500,
		DNS:       []string{"192.168.1.1"},
	}

	content := GenerateNetworkFile(cfg)

	if !strings.Contains(content, "[Match]\nName=eth0") {
		t.Error("missing match section")
	}
	if !strings.Contains(content, "Address=192.168.1.50/24") {
		t.Error("missing IPv4 address")
	}
	if !strings.Contains(content, "Gateway=192.168.1.1") {
		t.Error("missing IPv4 gateway")
	}
	if !strings.Contains(content, "Address=fd00::50/64") {
		t.Error("missing IPv6 address")
	}
	if !strings.Contains(content, "Gateway=fd00::1") {
		t.Error("missing IPv6 gateway")
	}
	if !strings.Contains(content, "IPv6AcceptRA=false") {
		t.Error("SLAAC should be disabled")
	}
	if !strings.Contains(content, "DNS=192.168.1.1") {
		t.Error("missing DNS")
	}
	if !strings.Contains(content, "MTUBytes=1500") {
		t.Error("missing MTU")
	}
}

func TestGenerateNetworkFileDHCP(t *testing.T) {
	cfg := InterfaceConfig{
		Name:  "eth0",
		DHCP4: true,
		SLAAC: true,
		MTU:   9000,
	}

	content := GenerateNetworkFile(cfg)

	if !strings.Contains(content, "DHCP=ipv4") {
		t.Error("missing DHCP=ipv4")
	}
	if !strings.Contains(content, "IPv6AcceptRA=true") {
		t.Error("SLAAC should be enabled")
	}
	if !strings.Contains(content, "MTUBytes=9000") {
		t.Error("missing jumbo frame MTU")
	}
}

func TestGenerateNetworkFileDualStackDHCP(t *testing.T) {
	cfg := InterfaceConfig{
		Name:  "eth0",
		DHCP4: true,
		DHCP6: true,
	}

	content := GenerateNetworkFile(cfg)
	// When both are true, should output DHCP=yes
	if !strings.Contains(content, "DHCP=yes") {
		t.Error("dual-stack DHCP should output DHCP=yes")
	}
}

// --- Bond validation and generation ---

func TestValidateBondMode(t *testing.T) {
	valid := []string{"802.3ad", "balance-rr", "active-backup", "balance-xor", "balance-tlb", "balance-alb"}
	for _, mode := range valid {
		if err := ValidateBondMode(mode); err != nil {
			t.Errorf("expected valid bond mode %q, got error: %v", mode, err)
		}
	}

	invalid := []string{"", "lacp", "round-robin", "failover", "raid1"}
	for _, mode := range invalid {
		if err := ValidateBondMode(mode); err == nil {
			t.Errorf("expected invalid bond mode %q to fail", mode)
		}
	}
}

func TestValidateBondName(t *testing.T) {
	valid := []string{"bond0", "bond1", "bond-mgmt"}
	for _, name := range valid {
		if err := ValidateBondName(name); err != nil {
			t.Errorf("expected valid bond name %q, got error: %v", name, err)
		}
	}

	invalid := []string{"eth0", "br0", "notabond"}
	for _, name := range invalid {
		if err := ValidateBondName(name); err == nil {
			t.Errorf("expected invalid bond name %q to fail", name)
		}
	}
}

func TestGenerateBondNetdevLACP(t *testing.T) {
	bond := BondConfig{
		Name: "bond0",
		Mode: "802.3ad",
	}

	content := GenerateBondNetdev(bond)

	if !strings.Contains(content, "Name=bond0") {
		t.Error("missing bond name")
	}
	if !strings.Contains(content, "Kind=bond") {
		t.Error("missing Kind=bond")
	}
	if !strings.Contains(content, "Mode=802.3ad") {
		t.Error("missing LACP mode")
	}
	if !strings.Contains(content, "TransmitHashPolicy=layer3+4") {
		t.Error("missing hash policy for LACP")
	}
	if !strings.Contains(content, "LACPTransmitRate=fast") {
		t.Error("missing LACP transmit rate")
	}
}

func TestGenerateBondNetdevActiveBackup(t *testing.T) {
	bond := BondConfig{
		Name: "bond0",
		Mode: "active-backup",
	}

	content := GenerateBondNetdev(bond)

	if !strings.Contains(content, "Mode=active-backup") {
		t.Error("missing active-backup mode")
	}
	// Should NOT have LACP-specific settings.
	if strings.Contains(content, "TransmitHashPolicy") {
		t.Error("active-backup should not have TransmitHashPolicy")
	}
	if strings.Contains(content, "LACPTransmitRate") {
		t.Error("active-backup should not have LACPTransmitRate")
	}
	// Should still have MII monitoring.
	if !strings.Contains(content, "MIIMonitorSec=100ms") {
		t.Error("missing MII monitoring")
	}
}

func TestGenerateBondMemberNetwork(t *testing.T) {
	content := GenerateBondMemberNetwork("eth0", "bond0")

	if !strings.Contains(content, "Name=eth0") {
		t.Error("missing member name")
	}
	if !strings.Contains(content, "Bond=bond0") {
		t.Error("missing bond reference")
	}
	// Should NOT have any IP config.
	if strings.Contains(content, "Address=") {
		t.Error("bond member should not have IP addresses")
	}
}

// --- VLAN validation and generation ---

func TestValidateVLANID(t *testing.T) {
	valid := []int{1, 100, 4094}
	for _, id := range valid {
		if err := ValidateVLANID(id); err != nil {
			t.Errorf("expected valid VLAN ID %d, got error: %v", id, err)
		}
	}

	invalid := []int{0, -1, 4095, 5000}
	for _, id := range invalid {
		if err := ValidateVLANID(id); err == nil {
			t.Errorf("expected invalid VLAN ID %d to fail", id)
		}
	}
}

func TestVLANName(t *testing.T) {
	name := VLANName("bond0", 100)
	if name != "bond0.100" {
		t.Errorf("expected bond0.100, got %s", name)
	}

	name = VLANName("eth0", 42)
	if name != "eth0.42" {
		t.Errorf("expected eth0.42, got %s", name)
	}
}

func TestGenerateVLANNetdev(t *testing.T) {
	vlan := VLANConfig{
		Name: "bond0.100",
		ID:   100,
	}

	content := GenerateVLANNetdev(vlan)

	if !strings.Contains(content, "Name=bond0.100") {
		t.Error("missing VLAN name")
	}
	if !strings.Contains(content, "Kind=vlan") {
		t.Error("missing Kind=vlan")
	}
	if !strings.Contains(content, "Id=100") {
		t.Error("missing VLAN ID")
	}
}

func TestGenerateVLANNetworkJumbo(t *testing.T) {
	vlan := VLANConfig{
		Name:      "bond0.100",
		IPv4Addrs: []string{"10.100.0.10/24"},
		MTU:       9000,
	}

	content := GenerateVLANNetwork(vlan)

	if !strings.Contains(content, "Address=10.100.0.10/24") {
		t.Error("missing VLAN IP")
	}
	if !strings.Contains(content, "MTUBytes=9000") {
		t.Error("missing jumbo MTU on VLAN")
	}
}

// --- DNS/hostname validation ---

func TestValidateHostname(t *testing.T) {
	valid := []string{"smoothnas", "myhost", "nas-01", "server1"}
	for _, name := range valid {
		if err := ValidateHostname(name); err != nil {
			t.Errorf("expected valid hostname %q, got error: %v", name, err)
		}
	}

	invalid := []string{"", "0bad", "-bad", "has space", "has;semi", strings.Repeat("a", 64)}
	for _, name := range invalid {
		if err := ValidateHostname(name); err == nil {
			t.Errorf("expected invalid hostname %q to fail", name)
		}
	}
}

func TestValidateDNSServer(t *testing.T) {
	valid := []string{"8.8.8.8", "192.168.1.1", "2001:4860:4860::8888"}
	for _, s := range valid {
		if err := ValidateDNSServer(s); err != nil {
			t.Errorf("expected valid DNS server %q, got error: %v", s, err)
		}
	}

	invalid := []string{"", "not-an-ip", "example.com"}
	for _, s := range invalid {
		if err := ValidateDNSServer(s); err == nil {
			t.Errorf("expected invalid DNS server %q to fail", s)
		}
	}
}

func TestValidateSearchDomain(t *testing.T) {
	valid := []string{"example.com", "local.lan", "home.arpa"}
	for _, d := range valid {
		if err := ValidateSearchDomain(d); err != nil {
			t.Errorf("expected valid search domain %q, got error: %v", d, err)
		}
	}

	invalid := []string{"", "has space.com", "-bad.com"}
	for _, d := range invalid {
		if err := ValidateSearchDomain(d); err == nil {
			t.Errorf("expected invalid search domain %q to fail", d)
		}
	}
}

func TestValidateRouteCIDR(t *testing.T) {
	valid := []string{"10.100.0.0/16", "192.168.0.0/24", "2001:db8::/32"}
	for _, cidr := range valid {
		if err := ValidateRouteCIDR(cidr); err != nil {
			t.Errorf("expected valid route CIDR %q, got error: %v", cidr, err)
		}
	}

	invalid := []string{"", "10.0.0.1", "not-cidr"}
	for _, cidr := range invalid {
		if err := ValidateRouteCIDR(cidr); err == nil {
			t.Errorf("expected invalid route CIDR %q to fail", cidr)
		}
	}
}

// --- Route section generation ---

func TestGenerateRouteSection(t *testing.T) {
	routes := []RouteConfig{
		{Destination: "10.100.0.0/16", Gateway: "192.168.1.1", Metric: 100},
		{Destination: "172.16.0.0/12", Gateway: "192.168.1.1"},
	}

	content := GenerateRouteSection(routes)

	if !strings.Contains(content, "[Route]") {
		t.Error("missing [Route] section")
	}
	if !strings.Contains(content, "Destination=10.100.0.0/16") {
		t.Error("missing first route destination")
	}
	if !strings.Contains(content, "Gateway=192.168.1.1") {
		t.Error("missing gateway")
	}
	if !strings.Contains(content, "Metric=100") {
		t.Error("missing metric")
	}
	if !strings.Contains(content, "Destination=172.16.0.0/12") {
		t.Error("missing second route")
	}
}

func TestGenerateRouteSectionEmpty(t *testing.T) {
	content := GenerateRouteSection(nil)
	if content != "" {
		t.Errorf("expected empty string for no routes, got: %s", content)
	}
}

// --- Safe-apply ---

// testSafeApply creates a SafeApply with temp dirs and a no-op reload.
func testSafeApply(t *testing.T) *SafeApply {
	t.Helper()
	netDir := t.TempDir()
	backDir := t.TempDir()
	noopReload := func() error { return nil }
	return NewSafeApplyWithDirs(netDir, backDir, noopReload)
}

func TestSafeApplyLifecycle(t *testing.T) {
	sa := testSafeApply(t)

	// Initially no pending change.
	if sa.IsPending() {
		t.Error("should not be pending initially")
	}
	if sa.Status() != nil {
		t.Error("status should be nil initially")
	}

	// Apply a change.
	err := sa.Apply("test change", func() error { return nil })
	if err != nil {
		t.Fatalf("apply error: %v", err)
	}

	if !sa.IsPending() {
		t.Error("should be pending after apply")
	}

	status := sa.Status()
	if status == nil {
		t.Fatal("status should not be nil after apply")
	}
	if status.Description != "test change" {
		t.Errorf("expected description 'test change', got %q", status.Description)
	}
	if status.Remaining <= 0 || status.Remaining > 90 {
		t.Errorf("remaining should be 1-90, got %d", status.Remaining)
	}

	// Confirm.
	err = sa.Confirm()
	if err != nil {
		t.Fatalf("confirm error: %v", err)
	}

	if sa.IsPending() {
		t.Error("should not be pending after confirm")
	}
}

func TestSafeApplyRevert(t *testing.T) {
	sa := testSafeApply(t)

	err := sa.Apply("test revert", func() error { return nil })
	if err != nil {
		t.Fatalf("apply error: %v", err)
	}

	err = sa.Revert()
	if err != nil {
		t.Fatalf("revert error: %v", err)
	}

	if sa.IsPending() {
		t.Error("should not be pending after revert")
	}
}

func TestSafeApplyDoubleApply(t *testing.T) {
	sa := testSafeApply(t)

	sa.Apply("first", func() error { return nil })

	// Second apply should fail while first is pending.
	err := sa.Apply("second", func() error { return nil })
	if err == nil {
		t.Error("expected error for double apply")
	}
	if !strings.Contains(err.Error(), "already pending") {
		t.Errorf("expected 'already pending' error, got: %v", err)
	}

	// Clean up.
	sa.Confirm()
}

func TestSafeApplyConfirmWithoutPending(t *testing.T) {
	sa := testSafeApply(t)

	err := sa.Confirm()
	if err == nil {
		t.Error("expected error when confirming without pending")
	}
}

func TestSafeApplyRevertWithoutPending(t *testing.T) {
	sa := testSafeApply(t)

	err := sa.Revert()
	if err == nil {
		t.Error("expected error when reverting without pending")
	}
}

func TestSafeApplyBackupRestore(t *testing.T) {
	netDir := t.TempDir()
	backDir := t.TempDir()
	noopReload := func() error { return nil }
	sa := NewSafeApplyWithDirs(netDir, backDir, noopReload)

	// Write a config file to the network dir.
	os.WriteFile(filepath.Join(netDir, "10-eth0.network"), []byte("original"), 0644)

	// Apply a change that overwrites it.
	err := sa.Apply("overwrite", func() error {
		return os.WriteFile(filepath.Join(netDir, "10-eth0.network"), []byte("modified"), 0644)
	})
	if err != nil {
		t.Fatalf("apply error: %v", err)
	}

	// Verify the file was modified.
	data, _ := os.ReadFile(filepath.Join(netDir, "10-eth0.network"))
	if string(data) != "modified" {
		t.Fatalf("expected modified, got %s", string(data))
	}

	// Revert should restore original.
	if err := sa.Revert(); err != nil {
		t.Fatalf("revert error: %v", err)
	}

	data, _ = os.ReadFile(filepath.Join(netDir, "10-eth0.network"))
	if string(data) != "original" {
		t.Fatalf("expected original after revert, got %s", string(data))
	}
}
