package network

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Default-bond policy (Phase 8 of the multi-NIC proposal).
//
// On a fresh install with no recorded network config, tierd materialises
// a bond0 across every physical Ethernet NIC in balance-alb mode, DHCPed.
// The bond's catch-all member file uses a wildcard [Match] so a
// previously-unconnected NIC plugged in later joins the bond
// automatically (one miimon cycle).
//
// The "default policy applied" marker (NetworkBootstrapCompleteKey) is
// set once on first successful apply and never auto-cleared. Subsequent
// reconciles are no-ops, so an operator's Break Bond / static-IP /
// custom-bond intent is preserved across tierd restarts.

const (
	// DefaultBondName is the name of the default appliance bond.
	DefaultBondName = "bond0"

	// DefaultBondMode is the bond mode used for the default policy.
	// balance-alb gives one shared IP, per-peer ARP load balancing
	// across NICs, no switch config required.
	DefaultBondMode = "balance-alb"

	// NetworkBootstrapCompleteKey is the config-store key used to
	// mark that the default-bond policy has been applied. The value
	// is "1" once set; absence means "not yet applied".
	NetworkBootstrapCompleteKey = "network.bootstrap_complete"

	// DefaultBondNetdevFilename is the systemd-networkd .netdev
	// filename written for the default bond device.
	DefaultBondNetdevFilename = "90-default-bond0.netdev"

	// DefaultBondNetworkFilename is the .network filename for the
	// bond's IP config (DHCP).
	DefaultBondNetworkFilename = "90-default-bond0.network"

	// DefaultBondMembersFilename is the catch-all member file. Its
	// 99- prefix puts it after any operator-written per-NIC config
	// (10-<name>.network) so explicit operator configs win and only
	// otherwise-unconfigured Ethernet NICs join the bond.
	DefaultBondMembersFilename = "99-default-bond-members.network"
)

// ConfigStore is the subset of *db.Store the policy code uses. Defined
// as an interface here so tests can stub it without pulling in the db
// package's migration machinery.
type ConfigStore interface {
	GetBoolConfig(key string, def bool) (bool, error)
	SetBoolConfig(key string, value bool) error
}

// IsPhysicalEthernet returns true if the named interface under sysRoot
// (typically "/sys/class/net") is a physical Ethernet NIC: real
// hardware with an ARPHRD_ETHER type, no wireless capability, and not
// a bond / bridge / vlan / tunnel / loopback / docker bridge.
//
// The check looks at four sysfs files:
//
//   - <name>/device     present iff backed by real hardware
//                       (filters out tunnels, bonds, bridges, veths)
//   - <name>/wireless   present iff Wi-Fi (filtered out)
//   - <name>/type       must read "1" (ARPHRD_ETHER)
//   - <name>            non-empty name (skips "" / "lo" / "bond*")
func IsPhysicalEthernet(sysRoot, name string) bool {
	if name == "" || name == "lo" {
		return false
	}
	// Filter virtual / docker / vmbr / veth name patterns up front
	// so we don't even stat the sysfs entries.
	for _, prefix := range []string{
		"bond", "br-", "br0", "virbr", "veth", "vnet",
		"docker", "tap", "tun", "wg", "ppp",
	} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	if strings.Contains(name, ".") {
		// Standard VLAN naming: <parent>.<vid>
		return false
	}
	base := filepath.Join(sysRoot, name)
	if st, err := os.Stat(filepath.Join(base, "device")); err != nil || !st.IsDir() && (st.Mode()&os.ModeSymlink) == 0 {
		// `device` is a symlink in real sysfs; in test fixtures we
		// accept any kind of present entry.
		_ = st
		if _, err := os.Lstat(filepath.Join(base, "device")); err != nil {
			return false
		}
	}
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return false
	}
	typeBytes, err := os.ReadFile(filepath.Join(base, "type"))
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(typeBytes)) != "1" {
		// 1 == ARPHRD_ETHER. Anything else (e.g. 772 loopback,
		// 776 IPv6-in-IPv4 tunnel, 778 GRE, 824 TEAM) is not what
		// we're looking for.
		return false
	}
	return true
}

// EnumeratePhysicalEthernet returns the names of physical Ethernet
// NICs found under sysRoot, sorted alphabetically. Suitable as the
// member list for the default bond policy.
func EnumeratePhysicalEthernet(sysRoot string) ([]string, error) {
	entries, err := os.ReadDir(sysRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sysRoot, err)
	}
	var names []string
	for _, e := range entries {
		if !IsPhysicalEthernet(sysRoot, e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// DefaultBondPolicy returns the canonical default bond config: bond0
// over the given members in balance-alb mode, DHCPed for IPv4. The
// members list is purely informational at this layer — the
// systemd-networkd member file uses a wildcard match so future NICs
// auto-join the bond without an operator action.
func DefaultBondPolicy(members []string) BondConfig {
	return BondConfig{
		Name:    DefaultBondName,
		Mode:    DefaultBondMode,
		Members: members,
		DHCP4:   true,
	}
}

// GenerateDefaultBondMembersNetwork returns the systemd-networkd
// .network file content for the catch-all bond-member match. It
// matches every ARPHRD_ETHER device that isn't already named like
// a bond / bridge / virtual / wireless interface, and binds them to
// the default bond.
//
// Operator-written per-NIC configs at lower file-prefix numbers
// (10-<name>.network with Match Name=<name>) override this catch-all
// because systemd-networkd evaluates files in lexical order and uses
// the first matching one.
func GenerateDefaultBondMembersNetwork() string {
	var b strings.Builder
	b.WriteString("# Auto-generated by tierd default-bond policy. Do not edit.\n")
	b.WriteString("# Catches every physical Ethernet NIC not explicitly\n")
	b.WriteString("# configured at a lower file-prefix number, so newly\n")
	b.WriteString("# plugged-in NICs join the default bond automatically.\n")
	b.WriteString("[Match]\n")
	b.WriteString("Type=ether\n")
	b.WriteString("Kind=!bond\n")
	b.WriteString("Name=!lo !bond* !br-* !br0 !virbr* !veth* !vnet* !docker* !tap* !tun* !wg* !ppp*\n")
	b.WriteString("\n")
	b.WriteString("[Network]\n")
	fmt.Fprintf(&b, "Bond=%s\n", DefaultBondName)
	return b.String()
}

// ApplyDefaultBondPolicy is the bootstrap reconcile entry point. On a
// fresh install (NetworkBootstrapCompleteKey unset), it enumerates
// physical Ethernet NICs and writes the default-bond systemd-networkd
// files. It is a no-op once the marker is set so an operator's Break
// Bond / static-IP / custom-bond intent is preserved across tierd
// restarts.
//
// A box with zero physical Ethernet NICs at first boot still flips the
// marker — the appliance has no NICs to bond, and rerunning the
// enumerate-and-bond on every restart would be a quiet trap (a NIC
// plugged in later would unexpectedly get auto-bonded). Operators with
// no NICs at first boot use the GUI to add a bond once cabling is in.
func ApplyDefaultBondPolicy(store ConfigStore, networkDir, sysRoot string) error {
	done, err := store.GetBoolConfig(NetworkBootstrapCompleteKey, false)
	if err != nil {
		return fmt.Errorf("read bootstrap marker: %w", err)
	}
	if done {
		return nil
	}
	return writeDefaultBondFiles(store, networkDir, sysRoot)
}

// writeDefaultBondFiles writes the three default-bond systemd-networkd
// files and sets the bootstrap marker. Used by ApplyDefaultBondPolicy
// (the bootstrap-marker-gated variant) and RecreateDefaultBond (the
// operator-driven variant that bypasses the marker).
func writeDefaultBondFiles(store ConfigStore, networkDir, sysRoot string) error {
	members, err := EnumeratePhysicalEthernet(sysRoot)
	if err != nil {
		return fmt.Errorf("enumerate physical ethernet: %w", err)
	}

	bond := DefaultBondPolicy(members)
	if err := WriteConfigFile(networkDir, DefaultBondNetdevFilename,
		GenerateBondNetdev(bond)); err != nil {
		return fmt.Errorf("write default bond netdev: %w", err)
	}
	if err := WriteConfigFile(networkDir, DefaultBondNetworkFilename,
		GenerateBondNetwork(bond)); err != nil {
		return fmt.Errorf("write default bond network: %w", err)
	}
	if err := WriteConfigFile(networkDir, DefaultBondMembersFilename,
		GenerateDefaultBondMembersNetwork()); err != nil {
		return fmt.Errorf("write default bond members: %w", err)
	}

	if err := store.SetBoolConfig(NetworkBootstrapCompleteKey, true); err != nil {
		return fmt.Errorf("set bootstrap marker: %w", err)
	}
	return nil
}

// BreakBond drops a bond and gives every member back its own
// per-NIC `.network` file (DHCP). Removes the bond's `.netdev` +
// `.network` files at filename prefix `05-` / `10-` (the prefixes
// the create/update bond paths in api/network.go use), and removes
// the default-bond catch-all member file at `99-default-bond-...`
// so newly-plugged NICs no longer auto-join the bond.
//
// Per-member configs are written at `10-<member>.network` with
// DHCP4 = true. Pre-bond static configs are not currently restored
// — the proposal allows DHCP-default for the v1 implementation;
// static-config persistence is deferred behind an operator-pain
// signal.
//
// The bootstrap marker is left set; the operator's Break Bond is
// what we want preserved across tierd restarts. RecreateDefaultBond
// is the explicit knob to bring the bond back.
func BreakBond(networkDir, bondName string, members []string) error {
	for _, m := range members {
		if err := ValidateInterfaceName(m); err != nil {
			return err
		}
	}
	if err := ValidateBondName(bondName); err != nil {
		return err
	}

	// Remove the bond's own files. Operator-created bonds live at
	// `05-<name>.netdev` / `10-<name>.network`; the appliance default
	// bond lives at `90-default-<name>.netdev` / `90-default-<name>.network`.
	// Both shells get cleaned so Break Bond works regardless of how
	// the bond was provisioned.
	prefixes := []string{
		"05-" + bondName + ".",
		"10-" + bondName + ".",
		"90-default-" + bondName + ".",
	}
	for _, p := range prefixes {
		if err := RemoveConfigFiles(networkDir, p); err != nil {
			return fmt.Errorf("remove %s files: %w", p, err)
		}
	}

	// Drop the catch-all that the default-bond policy uses to scoop
	// up unconfigured Ethernet NICs. Without this step a freshly-
	// plugged NIC after Break Bond would still try to join `bond0`,
	// which no longer exists.
	if err := RemoveConfigFiles(networkDir, DefaultBondMembersFilename); err != nil {
		return fmt.Errorf("remove default-bond members file: %w", err)
	}

	// Rewrite each member as a standalone DHCPed NIC. This
	// overwrites the bond-member `.network` file the create/update
	// bond paths wrote (Bond=<bondName>) with a Network=DHCP config
	// so the kernel releases the NIC from the bond.
	for _, m := range members {
		cfg := InterfaceConfig{Name: m, DHCP4: true}
		if err := WriteConfigFile(networkDir, "10-"+m+".network",
			GenerateNetworkFile(cfg)); err != nil {
			return fmt.Errorf("write per-member network for %s: %w", m, err)
		}
	}
	return nil
}

// ListActiveIPv4 returns the IPv4 addresses bound to bond / NIC /
// VLAN interfaces, with the CIDR suffix stripped so the result is
// suitable for Samba's `interfaces = ...` directive and similar
// protocol-side advertisements. Skips loopback. Best-effort:
// returns an empty slice if the underlying `ip` probe fails so an
// unreachable network layer doesn't block protocol-config writes.
func ListActiveIPv4() []string {
	ifaces, err := ListInterfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range ifaces {
		if i.Name == "lo" {
			continue
		}
		for _, cidr := range i.IPv4Addrs {
			if addr := stripCIDR(cidr); addr != "" {
				out = append(out, addr)
			}
		}
	}
	return out
}

func stripCIDR(cidr string) string {
	for i := 0; i < len(cidr); i++ {
		if cidr[i] == '/' {
			return cidr[:i]
		}
	}
	return cidr
}

// RecreateDefaultBond rebuilds the appliance default: a single
// `bond0` over every physical Ethernet NIC in `balance-alb` mode,
// DHCPed. Drops every per-NIC `.network` file the operator might
// have set after Break Bond (we can't tell operator-set from
// auto-generated, so the explicit Re-create gesture is destructive
// by design; the proposal documents this).
//
// Bypasses the bootstrap-marker check by writing the default-bond
// files directly via writeDefaultBondFiles. The marker is set on
// success so subsequent tierd restarts don't wipe a per-NIC config
// the operator might add later.
func RecreateDefaultBond(store ConfigStore, networkDir, sysRoot string) error {
	members, err := EnumeratePhysicalEthernet(sysRoot)
	if err != nil {
		return fmt.Errorf("enumerate physical ethernet: %w", err)
	}

	for _, m := range members {
		// Best-effort cleanup; if the file doesn't exist (operator
		// never set per-NIC config), RemoveConfigFiles is a no-op.
		_ = RemoveConfigFiles(networkDir, "10-"+m+".")
	}
	return writeDefaultBondFiles(store, networkDir, sysRoot)
}
