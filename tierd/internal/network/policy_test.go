package network

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubConfigStore is the in-memory ConfigStore used in tests.
type stubConfigStore struct {
	bools   map[string]bool
	getErr  error
	setErr  error
	setKeys []string
}

func newStubConfigStore() *stubConfigStore {
	return &stubConfigStore{bools: map[string]bool{}}
}

func (s *stubConfigStore) GetBoolConfig(key string, def bool) (bool, error) {
	if s.getErr != nil {
		return false, s.getErr
	}
	v, ok := s.bools[key]
	if !ok {
		return def, nil
	}
	return v, nil
}

func (s *stubConfigStore) SetBoolConfig(key string, value bool) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.bools[key] = value
	s.setKeys = append(s.setKeys, key)
	return nil
}

// makeFakeSysClassNet builds a /sys/class/net-shaped tree at root with
// the given specs. Each spec is a map describing one interface:
//
//	"physical-eth": create files device + type=1 (real Ethernet)
//	"wireless":     create files device + type=1 + wireless dir
//	"loopback":     create files type=772 (no device)
//	"virtual":      no device, no type
//	"tunnel":       device + type=778 (GRE)
//
// Returns root path.
func makeFakeSysClassNet(t *testing.T, specs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, kind := range specs {
		base := filepath.Join(root, name)
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", base, err)
		}
		switch kind {
		case "physical-eth":
			writeFakeFile(t, filepath.Join(base, "device"), "")
			writeFakeFile(t, filepath.Join(base, "type"), "1\n")
		case "wireless":
			writeFakeFile(t, filepath.Join(base, "device"), "")
			writeFakeFile(t, filepath.Join(base, "type"), "1\n")
			if err := os.MkdirAll(filepath.Join(base, "wireless"), 0o755); err != nil {
				t.Fatalf("mkdir wireless: %v", err)
			}
		case "loopback":
			// Loopback interfaces have no `device` symlink but do
			// have type 772.
			writeFakeFile(t, filepath.Join(base, "type"), "772\n")
		case "virtual":
			// e.g. a bridge or bond before it has members.
			writeFakeFile(t, filepath.Join(base, "type"), "1\n")
			// no device file
		case "tunnel":
			writeFakeFile(t, filepath.Join(base, "device"), "")
			writeFakeFile(t, filepath.Join(base, "type"), "778\n")
		default:
			t.Fatalf("unknown spec kind %q", kind)
		}
	}
	return root
}

func writeFakeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestIsPhysicalEthernetAcceptsRealEthernet(t *testing.T) {
	root := makeFakeSysClassNet(t, map[string]string{
		"enp1s0": "physical-eth",
		"enp2s0": "physical-eth",
	})
	if !IsPhysicalEthernet(root, "enp1s0") {
		t.Fatalf("enp1s0 should be physical Ethernet")
	}
	if !IsPhysicalEthernet(root, "enp2s0") {
		t.Fatalf("enp2s0 should be physical Ethernet")
	}
}

func TestIsPhysicalEthernetRejectsLoopback(t *testing.T) {
	root := makeFakeSysClassNet(t, map[string]string{
		"lo": "loopback",
	})
	if IsPhysicalEthernet(root, "lo") {
		t.Fatalf("lo must not be physical Ethernet")
	}
}

func TestIsPhysicalEthernetRejectsWireless(t *testing.T) {
	root := makeFakeSysClassNet(t, map[string]string{
		"wlan0":  "wireless",
		"wlp3s0": "wireless",
	})
	if IsPhysicalEthernet(root, "wlan0") {
		t.Fatalf("wlan0 must not be physical Ethernet")
	}
	if IsPhysicalEthernet(root, "wlp3s0") {
		t.Fatalf("wlp3s0 must not be physical Ethernet")
	}
}

func TestIsPhysicalEthernetRejectsVirtual(t *testing.T) {
	root := makeFakeSysClassNet(t, map[string]string{
		"bond0":   "virtual",
		"bond1":   "virtual",
		"br0":     "virtual",
		"virbr0":  "virtual",
		"veth1":   "virtual",
		"docker0": "virtual",
		"tun0":    "virtual",
	})
	for _, name := range []string{"bond0", "bond1", "br0", "virbr0", "veth1", "docker0", "tun0"} {
		if IsPhysicalEthernet(root, name) {
			t.Fatalf("%s must not be physical Ethernet (virtual / bond / bridge)", name)
		}
	}
}

func TestIsPhysicalEthernetRejectsTunnel(t *testing.T) {
	root := makeFakeSysClassNet(t, map[string]string{
		"gre0": "tunnel",
	})
	if IsPhysicalEthernet(root, "gre0") {
		t.Fatalf("gre0 must not be physical Ethernet (ARPHRD_IPGRE)")
	}
}

func TestIsPhysicalEthernetRejectsVLANNamingPattern(t *testing.T) {
	root := makeFakeSysClassNet(t, map[string]string{
		"enp1s0.100": "physical-eth", // dotted name == VLAN
	})
	if IsPhysicalEthernet(root, "enp1s0.100") {
		t.Fatalf("dotted VLAN names must not be picked up as physical")
	}
}

func TestEnumeratePhysicalEthernetSorted(t *testing.T) {
	root := makeFakeSysClassNet(t, map[string]string{
		"enp3s0":   "physical-eth",
		"enp1s0":   "physical-eth",
		"enp2s0":   "physical-eth",
		"lo":       "loopback",
		"wlan0":    "wireless",
		"bond0":    "virtual",
		"docker0":  "virtual",
	})
	got, err := EnumeratePhysicalEthernet(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	want := []string{"enp1s0", "enp2s0", "enp3s0"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestEnumeratePhysicalEthernetEmpty(t *testing.T) {
	root := t.TempDir() // no entries
	got, err := EnumeratePhysicalEthernet(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestDefaultBondPolicyShape(t *testing.T) {
	bond := DefaultBondPolicy([]string{"enp1s0", "enp2s0"})
	if bond.Name != DefaultBondName {
		t.Fatalf("name = %q, want %q", bond.Name, DefaultBondName)
	}
	if bond.Mode != DefaultBondMode {
		t.Fatalf("mode = %q, want %q", bond.Mode, DefaultBondMode)
	}
	if !bond.DHCP4 {
		t.Fatalf("DHCP4 should be true by default")
	}
	if bond.DHCP6 || bond.SLAAC {
		t.Fatalf("DHCP6/SLAAC should be off by default; got DHCP6=%v SLAAC=%v", bond.DHCP6, bond.SLAAC)
	}
	if len(bond.Members) != 2 || bond.Members[0] != "enp1s0" || bond.Members[1] != "enp2s0" {
		t.Fatalf("members = %v, want [enp1s0 enp2s0]", bond.Members)
	}
}

func TestDefaultBondPolicyValidatesMode(t *testing.T) {
	bond := DefaultBondPolicy(nil)
	if err := ValidateBondMode(bond.Mode); err != nil {
		t.Fatalf("default mode rejected by validator: %v", err)
	}
}

func TestGenerateDefaultBondMembersNetworkSyntax(t *testing.T) {
	got := GenerateDefaultBondMembersNetwork()
	for _, want := range []string{
		"[Match]",
		"Type=ether",
		"Kind=!bond",
		"!bond*",
		"!virbr*",
		"!docker*",
		"[Network]",
		"Bond=" + DefaultBondName,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("members file missing %q\n%s", want, got)
		}
	}
}

func TestApplyDefaultBondPolicyHappyPath(t *testing.T) {
	store := newStubConfigStore()
	netDir := t.TempDir()
	sysRoot := makeFakeSysClassNet(t, map[string]string{
		"enp1s0": "physical-eth",
		"enp2s0": "physical-eth",
		"lo":     "loopback",
		"wlan0":  "wireless",
	})

	if err := ApplyDefaultBondPolicy(store, netDir, sysRoot); err != nil {
		t.Fatalf("apply: %v", err)
	}

	for _, fn := range []string{
		DefaultBondNetdevFilename,
		DefaultBondNetworkFilename,
		DefaultBondMembersFilename,
	} {
		path := filepath.Join(netDir, fn)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if len(data) == 0 {
			t.Fatalf("file %s is empty", path)
		}
	}

	netdev, _ := os.ReadFile(filepath.Join(netDir, DefaultBondNetdevFilename))
	if !strings.Contains(string(netdev), "Mode="+DefaultBondMode) {
		t.Fatalf("netdev missing mode %q\n%s", DefaultBondMode, netdev)
	}
	bondNetwork, _ := os.ReadFile(filepath.Join(netDir, DefaultBondNetworkFilename))
	if !strings.Contains(string(bondNetwork), "DHCP=ipv4") {
		t.Fatalf("bond network missing DHCP=ipv4\n%s", bondNetwork)
	}
	members, _ := os.ReadFile(filepath.Join(netDir, DefaultBondMembersFilename))
	if !strings.Contains(string(members), "Bond="+DefaultBondName) {
		t.Fatalf("members file missing Bond= directive\n%s", members)
	}

	done, _ := store.GetBoolConfig(NetworkBootstrapCompleteKey, false)
	if !done {
		t.Fatalf("bootstrap marker not set after successful apply")
	}
}

func TestApplyDefaultBondPolicyIsNoOpAfterMarker(t *testing.T) {
	store := newStubConfigStore()
	store.bools[NetworkBootstrapCompleteKey] = true
	netDir := t.TempDir()
	sysRoot := makeFakeSysClassNet(t, map[string]string{
		"enp1s0": "physical-eth",
	})

	if err := ApplyDefaultBondPolicy(store, netDir, sysRoot); err != nil {
		t.Fatalf("apply: %v", err)
	}

	entries, _ := os.ReadDir(netDir)
	if len(entries) != 0 {
		t.Fatalf("apply should be a no-op after marker; got files %v", entries)
	}
}

func TestApplyDefaultBondPolicyEmptyNICSetStillSetsMarker(t *testing.T) {
	store := newStubConfigStore()
	netDir := t.TempDir()
	sysRoot := t.TempDir() // zero NICs

	if err := ApplyDefaultBondPolicy(store, netDir, sysRoot); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Files are still written — the catch-all member match doesn't
	// require pre-enumeration, so newly-plugged NICs auto-join.
	if _, err := os.Stat(filepath.Join(netDir, DefaultBondNetdevFilename)); err != nil {
		t.Fatalf("netdev file missing on zero-NIC apply: %v", err)
	}
	done, _ := store.GetBoolConfig(NetworkBootstrapCompleteKey, false)
	if !done {
		t.Fatalf("marker not set on zero-NIC apply")
	}
}

func TestApplyDefaultBondPolicyPropagatesGetMarkerErr(t *testing.T) {
	store := newStubConfigStore()
	store.getErr = errors.New("simulated store down")
	netDir := t.TempDir()
	sysRoot := t.TempDir()

	err := ApplyDefaultBondPolicy(store, netDir, sysRoot)
	if err == nil {
		t.Fatalf("expected error when store.GetBoolConfig fails")
	}
	if !strings.Contains(err.Error(), "bootstrap marker") {
		t.Fatalf("err = %v, want mentions bootstrap marker", err)
	}
}

func TestApplyDefaultBondPolicyRevertibleMarkerAfterFileWriteFailure(t *testing.T) {
	store := newStubConfigStore()
	// Pass a path that doesn't exist as networkDir to force WriteFile
	// to fail at the first attempt.
	netDir := filepath.Join(t.TempDir(), "does-not-exist")
	sysRoot := makeFakeSysClassNet(t, map[string]string{
		"enp1s0": "physical-eth",
	})

	if err := ApplyDefaultBondPolicy(store, netDir, sysRoot); err == nil {
		t.Fatalf("expected error when networkDir doesn't exist")
	}
	done, _ := store.GetBoolConfig(NetworkBootstrapCompleteKey, false)
	if done {
		t.Fatalf("bootstrap marker should NOT be set when file write failed")
	}
}

// applyDefaultBondAndAssertFiles is a helper that applies the default
// bond policy and asserts the three expected files were written.
// Returns the netDir for the caller to inspect or extend.
func applyDefaultBondAndAssertFiles(t *testing.T, store ConfigStore, sysRoot string) string {
	t.Helper()
	netDir := t.TempDir()
	if err := ApplyDefaultBondPolicy(store, netDir, sysRoot); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, fn := range []string{
		DefaultBondNetdevFilename,
		DefaultBondNetworkFilename,
		DefaultBondMembersFilename,
	} {
		if _, err := os.Stat(filepath.Join(netDir, fn)); err != nil {
			t.Fatalf("expected %s after apply: %v", fn, err)
		}
	}
	return netDir
}

func TestBreakBondRemovesBondFilesAndCatchAll(t *testing.T) {
	store := newStubConfigStore()
	sysRoot := makeFakeSysClassNet(t, map[string]string{
		"enp1s0": "physical-eth",
		"enp2s0": "physical-eth",
	})
	netDir := applyDefaultBondAndAssertFiles(t, store, sysRoot)

	if err := BreakBond(netDir, DefaultBondName, []string{"enp1s0", "enp2s0"}); err != nil {
		t.Fatalf("break: %v", err)
	}

	for _, fn := range []string{
		DefaultBondNetdevFilename,
		DefaultBondNetworkFilename,
		DefaultBondMembersFilename,
	} {
		if _, err := os.Stat(filepath.Join(netDir, fn)); err == nil {
			t.Fatalf("file %s should be gone after Break Bond", fn)
		}
	}
}

func TestBreakBondWritesDHCPPerMemberFiles(t *testing.T) {
	store := newStubConfigStore()
	sysRoot := makeFakeSysClassNet(t, map[string]string{
		"enp1s0": "physical-eth",
		"enp2s0": "physical-eth",
	})
	netDir := applyDefaultBondAndAssertFiles(t, store, sysRoot)

	if err := BreakBond(netDir, DefaultBondName, []string{"enp1s0", "enp2s0"}); err != nil {
		t.Fatalf("break: %v", err)
	}

	for _, m := range []string{"enp1s0", "enp2s0"} {
		path := filepath.Join(netDir, "10-"+m+".network")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("expected per-member file %s: %v", path, err)
		}
		text := string(data)
		if !strings.Contains(text, "Name="+m) {
			t.Fatalf("per-member file missing match for %s\n%s", m, text)
		}
		if !strings.Contains(text, "DHCP=ipv4") {
			t.Fatalf("per-member file %s missing DHCP=ipv4\n%s", m, text)
		}
		if strings.Contains(text, "Bond=") {
			t.Fatalf("per-member file %s should not still bind to a bond after break\n%s", m, text)
		}
	}
}

func TestBreakBondLeavesBootstrapMarkerSet(t *testing.T) {
	store := newStubConfigStore()
	sysRoot := makeFakeSysClassNet(t, map[string]string{
		"enp1s0": "physical-eth",
	})
	netDir := applyDefaultBondAndAssertFiles(t, store, sysRoot)

	if err := BreakBond(netDir, DefaultBondName, []string{"enp1s0"}); err != nil {
		t.Fatalf("break: %v", err)
	}

	done, _ := store.GetBoolConfig(NetworkBootstrapCompleteKey, false)
	if !done {
		t.Fatalf("bootstrap marker must remain set so Phase 1's startup reconcile is a no-op after Break Bond")
	}
}

func TestBreakBondRejectsBadName(t *testing.T) {
	if err := BreakBond(t.TempDir(), "not-a-bond", nil); err == nil {
		t.Fatalf("BreakBond accepted non-bond name")
	}
}

func TestBreakBondRejectsBadMember(t *testing.T) {
	if err := BreakBond(t.TempDir(), DefaultBondName, []string{"???bad-name"}); err == nil {
		t.Fatalf("BreakBond accepted invalid member name")
	}
}

func TestStripCIDR(t *testing.T) {
	cases := map[string]string{
		"192.168.1.10/24":  "192.168.1.10",
		"10.0.0.5/8":       "10.0.0.5",
		"2001:db8::10/64":  "2001:db8::10",
		"plain":            "plain",
		"":                 "",
		"no-slash-here":    "no-slash-here",
	}
	for in, want := range cases {
		if got := stripCIDR(in); got != want {
			t.Fatalf("stripCIDR(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecreateDefaultBondClearsPerNICFilesAndRewritesBond(t *testing.T) {
	store := newStubConfigStore()
	sysRoot := makeFakeSysClassNet(t, map[string]string{
		"enp1s0": "physical-eth",
		"enp2s0": "physical-eth",
	})
	netDir := applyDefaultBondAndAssertFiles(t, store, sysRoot)

	// Simulate operator path: Break Bond, then operator sets a static
	// IP on enp1s0. Recreate must wipe both per-NIC files.
	if err := BreakBond(netDir, DefaultBondName, []string{"enp1s0", "enp2s0"}); err != nil {
		t.Fatalf("break: %v", err)
	}
	staticConfig := InterfaceConfig{
		Name:      "enp1s0",
		IPv4Addrs: []string{"10.0.0.5/24"},
		Gateway4:  "10.0.0.1",
	}
	if err := WriteConfigFile(netDir, "10-enp1s0.network", GenerateNetworkFile(staticConfig)); err != nil {
		t.Fatalf("seed static config: %v", err)
	}

	if err := RecreateDefaultBond(store, netDir, sysRoot); err != nil {
		t.Fatalf("recreate: %v", err)
	}

	for _, m := range []string{"enp1s0", "enp2s0"} {
		path := filepath.Join(netDir, "10-"+m+".network")
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("per-NIC file %s should be removed after Re-create", path)
		}
	}
	for _, fn := range []string{
		DefaultBondNetdevFilename,
		DefaultBondNetworkFilename,
		DefaultBondMembersFilename,
	} {
		if _, err := os.Stat(filepath.Join(netDir, fn)); err != nil {
			t.Fatalf("expected default-bond file %s after Re-create: %v", fn, err)
		}
	}
}
