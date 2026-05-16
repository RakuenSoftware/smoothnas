// Package network manages network configuration via systemd-networkd.
//
// tierd owns all files in /etc/systemd/network/. It generates .network,
// .netdev, and .link files from its internal state. Manual edits are
// overwritten. Changes are applied via networkctl reload + reconfigure.
package network

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Interface represents a physical network interface.
type Interface struct {
	Name       string   `json:"name"` // e.g. "eth0", "enp3s0"
	MAC        string   `json:"mac"`
	State      string   `json:"state"` // "up", "down", "unknown"
	Speed      string   `json:"speed"` // e.g. "1000Mbps"
	MTU        int      `json:"mtu"`
	Driver     string   `json:"driver"`
	IPv4Addrs  []string `json:"ipv4_addrs"` // CIDR notation
	IPv6Addrs  []string `json:"ipv6_addrs"` // CIDR notation
	Gateway4   string   `json:"gateway4"`
	Gateway6   string   `json:"gateway6"`
	DHCP4      bool     `json:"dhcp4"`
	DHCP6      bool     `json:"dhcp6"`
	SLAAC      bool     `json:"slaac"`      // IPv6 SLAAC (accept RA)
	DNS        []string `json:"dns"`        // upstream DNS servers configured on this link
	Assignment string   `json:"assignment"` // "standalone", "bond-member", "unused"
	BondName   string   `json:"bond_name"`  // non-empty if bond member
}

var ifaceNameRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._-]{0,15}$`)

// ValidateInterfaceName checks that an interface name is safe.
func ValidateInterfaceName(name string) error {
	if !ifaceNameRegex.MatchString(name) {
		return fmt.Errorf("invalid interface name: %s", name)
	}
	return nil
}

// IsRuntimeInterfaceName reports interfaces created by local container,
// VM, tunnel, or VPN runtimes. These are not appliance NICs and should not
// be shown as configurable physical links.
func IsRuntimeInterfaceName(name string) bool {
	for _, prefix := range []string{
		"br-", "br0", "virbr", "veth", "vnet",
		"docker", "tap", "tun", "wg", "ppp",
		"gow",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// IsBondIneligibleInterfaceName is the name-pattern guard used before
// adding an interface to a systemd-networkd bond. It filters appliance
// virtual links in addition to runtime links.
func IsBondIneligibleInterfaceName(name string) bool {
	if name == "" || name == "lo" || strings.HasPrefix(name, "bond") {
		return true
	}
	if strings.Contains(name, ".") {
		// Standard VLAN naming: <parent>.<vid>
		return true
	}
	return IsRuntimeInterfaceName(name)
}

// ValidateIPv4CIDR validates an IPv4 address in CIDR notation.
var ipv4CIDRRegex = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}/\d{1,2}$`)

func ValidateIPv4CIDR(addr string) error {
	if !ipv4CIDRRegex.MatchString(addr) {
		return fmt.Errorf("invalid IPv4 CIDR: %s", addr)
	}
	return nil
}

// ValidateIPv6CIDR validates an IPv6 address in CIDR notation.
func ValidateIPv6CIDR(addr string) error {
	if !strings.Contains(addr, ":") || !strings.Contains(addr, "/") {
		return fmt.Errorf("invalid IPv6 CIDR: %s", addr)
	}
	return nil
}

// ValidateIPv4 validates a plain IPv4 address (no CIDR).
var ipv4Regex = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$`)

func ValidateIPv4(addr string) error {
	if !ipv4Regex.MatchString(addr) {
		return fmt.Errorf("invalid IPv4 address: %s", addr)
	}
	return nil
}

// ValidateMTU checks that MTU is in a reasonable range.
func ValidateMTU(mtu int) error {
	if mtu < 576 || mtu > 9000 {
		return fmt.Errorf("invalid MTU %d (must be 576-9000)", mtu)
	}
	return nil
}

// ListInterfaces discovers physical network interfaces via ip -j link show.
func ListInterfaces() ([]Interface, error) {
	return ListInterfacesWithConfig(defaultNetworkDir)
}

// ListInterfacesWithConfig discovers interfaces and overlays tierd-managed
// systemd-networkd settings, including DNS, from networkDir.
func ListInterfacesWithConfig(networkDir string) ([]Interface, error) {
	out, err := exec.Command("ip", "-j", "link", "show").Output()
	if err != nil {
		return nil, fmt.Errorf("ip link show: %w", err)
	}

	var raw []struct {
		Ifname    string `json:"ifname"`
		Address   string `json:"address"`
		Operstate string `json:"operstate"`
		Mtu       int    `json:"mtu"`
		LinkType  string `json:"link_type"`
		LinkInfo  struct {
			InfoKind string `json:"info_kind"`
		} `json:"linkinfo"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse ip output: %w", err)
	}

	var ifaces []Interface
	for _, r := range raw {
		// Skip loopback and virtual interfaces.
		if r.Ifname == "lo" || r.LinkType == "loopback" {
			continue
		}
		if shouldHideRuntimeInterface(r.Ifname, r.LinkInfo.InfoKind) {
			continue
		}

		iface := Interface{
			Name:       r.Ifname,
			MAC:        r.Address,
			State:      r.Operstate,
			MTU:        r.Mtu,
			Assignment: "standalone",
		}

		// Get IP addresses.
		iface.IPv4Addrs, iface.IPv6Addrs = getAddresses(r.Ifname)
		if settings, ok := readNetworkFileSettings(networkDir, r.Ifname); ok {
			iface.applySettings(settings)
		}

		// Get speed.
		speedOut, _ := exec.Command("cat", "/sys/class/net/"+r.Ifname+"/speed").Output()
		speed := strings.TrimSpace(string(speedOut))
		if speed != "" && speed != "-1" {
			iface.Speed = speed + "Mbps"
		}

		// Get driver.
		driverOut, _ := exec.Command("readlink", "/sys/class/net/"+r.Ifname+"/device/driver").Output()
		if driver := strings.TrimSpace(string(driverOut)); driver != "" {
			parts := strings.Split(driver, "/")
			iface.Driver = parts[len(parts)-1]
		}

		ifaces = append(ifaces, iface)
	}

	return ifaces, nil
}

func shouldHideRuntimeInterface(name, kind string) bool {
	switch kind {
	case "bridge", "veth", "tun", "tap", "dummy", "wireguard", "vxlan":
		return true
	}
	return IsRuntimeInterfaceName(name)
}

type networkFileSettings struct {
	IPv4Addrs []string
	IPv6Addrs []string
	Gateway4  string
	Gateway6  string
	DHCP4     bool
	DHCP6     bool
	SLAAC     bool
	MTU       int
	DNS       []string
}

func (iface *Interface) applySettings(settings networkFileSettings) {
	if len(settings.IPv4Addrs) > 0 {
		iface.IPv4Addrs = settings.IPv4Addrs
	}
	if len(settings.IPv6Addrs) > 0 {
		iface.IPv6Addrs = settings.IPv6Addrs
	}
	iface.Gateway4 = settings.Gateway4
	iface.Gateway6 = settings.Gateway6
	iface.DHCP4 = settings.DHCP4
	iface.DHCP6 = settings.DHCP6
	iface.SLAAC = settings.SLAAC
	if settings.MTU > 0 {
		iface.MTU = settings.MTU
	}
	iface.DNS = settings.DNS
}

func readNetworkFileSettings(networkDir, name string) (networkFileSettings, bool) {
	for _, filename := range networkFileCandidates(name) {
		data, err := os.ReadFile(filepath.Join(networkDir, filename))
		if err != nil {
			continue
		}
		return parseNetworkFileSettings(data), true
	}
	return networkFileSettings{}, false
}

func networkFileCandidates(name string) []string {
	candidates := []string{"10-" + name + ".network"}
	if name == DefaultBondName {
		candidates = append(candidates, DefaultBondNetworkFilename)
	}
	return candidates
}

func parseNetworkFileSettings(data []byte) networkFileSettings {
	var settings networkFileSettings
	section := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch section {
		case "Network":
			applyNetworkSetting(&settings, key, value)
		case "Link":
			if key == "MTUBytes" {
				if mtu, err := strconv.Atoi(value); err == nil {
					settings.MTU = mtu
				}
			}
		}
	}
	return settings
}

func applyNetworkSetting(settings *networkFileSettings, key, value string) {
	switch key {
	case "Address":
		if strings.Contains(value, ":") {
			settings.IPv6Addrs = append(settings.IPv6Addrs, value)
		} else {
			settings.IPv4Addrs = append(settings.IPv4Addrs, value)
		}
	case "Gateway":
		if strings.Contains(value, ":") {
			settings.Gateway6 = value
		} else {
			settings.Gateway4 = value
		}
	case "DHCP":
		switch strings.ToLower(value) {
		case "yes", "true":
			settings.DHCP4 = true
			settings.DHCP6 = true
		case "ipv4":
			settings.DHCP4 = true
		case "ipv6":
			settings.DHCP6 = true
		}
	case "IPv6AcceptRA":
		settings.SLAAC = value == "true" || value == "yes"
	case "DNS":
		settings.DNS = append(settings.DNS, value)
	}
}

// getAddresses returns IPv4 and IPv6 addresses for an interface.
func getAddresses(ifname string) (ipv4, ipv6 []string) {
	out, err := exec.Command("ip", "-j", "addr", "show", ifname).Output()
	if err != nil {
		return
	}

	var raw []struct {
		AddrInfo []struct {
			Family    string `json:"family"`
			Local     string `json:"local"`
			Prefixlen int    `json:"prefixlen"`
			Scope     string `json:"scope"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return
	}

	for _, r := range raw {
		for _, a := range r.AddrInfo {
			cidr := fmt.Sprintf("%s/%d", a.Local, a.Prefixlen)
			switch a.Family {
			case "inet":
				ipv4 = append(ipv4, cidr)
			case "inet6":
				if a.Scope != "link" { // skip link-local
					ipv6 = append(ipv6, cidr)
				}
			}
		}
	}
	return
}

// GenerateLinkFile generates a systemd-networkd .link file that pins a MAC
// address to a persistent interface name. This ensures NIC names survive
// hardware changes (adding/removing devices).
func GenerateLinkFile(name, mac string) string {
	var b strings.Builder
	b.WriteString("# Auto-generated by tierd. Do not edit.\n")
	fmt.Fprintf(&b, "[Match]\nMACAddress=%s\n\n", mac)
	fmt.Fprintf(&b, "[Link]\nName=%s\n", name)
	return b.String()
}

// GenerateNetworkFile generates a systemd-networkd .network file for a standalone interface.
func GenerateNetworkFile(iface InterfaceConfig) string {
	var b strings.Builder

	b.WriteString("# Auto-generated by tierd. Do not edit.\n")
	fmt.Fprintf(&b, "[Match]\nName=%s\n\n", iface.Name)

	b.WriteString("[Network]\n")

	switch {
	case iface.DHCP4 && iface.DHCP6:
		b.WriteString("DHCP=yes\n")
	case iface.DHCP4:
		b.WriteString("DHCP=ipv4\n")
	case iface.DHCP6:
		b.WriteString("DHCP=ipv6\n")
	}

	for _, addr := range iface.IPv4Addrs {
		fmt.Fprintf(&b, "Address=%s\n", addr)
	}
	if iface.Gateway4 != "" {
		fmt.Fprintf(&b, "Gateway=%s\n", iface.Gateway4)
	}
	for _, addr := range iface.IPv6Addrs {
		fmt.Fprintf(&b, "Address=%s\n", addr)
	}
	if iface.Gateway6 != "" {
		fmt.Fprintf(&b, "Gateway=%s\n", iface.Gateway6)
	}

	if iface.SLAAC {
		b.WriteString("IPv6AcceptRA=true\n")
	} else {
		b.WriteString("IPv6AcceptRA=false\n")
	}

	for _, dns := range iface.DNS {
		fmt.Fprintf(&b, "DNS=%s\n", dns)
	}

	b.WriteString("\n[Link]\n")
	if iface.MTU > 0 {
		fmt.Fprintf(&b, "MTUBytes=%d\n", iface.MTU)
	}

	return b.String()
}

// InterfaceConfig holds the desired configuration for an interface.
type InterfaceConfig struct {
	Name      string   `json:"name"`
	IPv4Addrs []string `json:"ipv4_addrs"`
	IPv6Addrs []string `json:"ipv6_addrs"`
	Gateway4  string   `json:"gateway4"`
	Gateway6  string   `json:"gateway6"`
	DHCP4     bool     `json:"dhcp4"`
	DHCP6     bool     `json:"dhcp6"`
	SLAAC     bool     `json:"slaac"`
	MTU       int      `json:"mtu"`
	DNS       []string `json:"dns"`
}

// WriteConfigFile writes content to /etc/systemd/network/{filename}.
func WriteConfigFile(networkDir, filename, content string) error {
	return os.WriteFile(filepath.Join(networkDir, filename), []byte(content), 0644)
}

// RemoveLegacyCatchAllDHCP removes the old installer fallback DHCP file when
// it is still the broad Type=ether catch-all. Specific tierd-generated network
// files use the same 10- prefix, so leaving this file in place makes static IP
// and bond-member configs inert under systemd-networkd's first-match rule.
func RemoveLegacyCatchAllDHCP(networkDir string) error {
	path := filepath.Join(networkDir, LegacyDHCPNetworkFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !isLegacyCatchAllDHCP(data) {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func isLegacyCatchAllDHCP(data []byte) bool {
	lines := strings.Split(string(data), "\n")
	inMatch := false
	inNetwork := false
	matchTypeEther := false
	matchHasSpecificSelector := false
	networkDHCP := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch line {
		case "[Match]":
			inMatch = true
			inNetwork = false
			continue
		case "[Network]":
			inMatch = false
			inNetwork = true
			continue
		default:
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				inMatch = false
				inNetwork = false
			}
		}
		if inMatch {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				matchHasSpecificSelector = true
				continue
			}
			if strings.TrimSpace(key) == "Type" && strings.TrimSpace(value) == "ether" {
				matchTypeEther = true
			} else {
				matchHasSpecificSelector = true
			}
		}
		if inNetwork && line == "DHCP=yes" {
			networkDHCP = true
		}
	}
	return matchTypeEther && !matchHasSpecificSelector && networkDHCP
}

// RemoveConfigFiles removes files matching a prefix from the network dir.
func RemoveConfigFiles(networkDir, prefix string) error {
	entries, err := os.ReadDir(networkDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			if err := os.Remove(filepath.Join(networkDir, e.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// ListBonds discovers bond interfaces from the system.
func ListBonds() ([]BondConfig, error) {
	return ListBondsWithConfig(defaultNetworkDir)
}

// ListBondsWithConfig discovers bonds and overlays tierd-managed networkd settings.
func ListBondsWithConfig(networkDir string) ([]BondConfig, error) {
	out, err := exec.Command("ip", "-j", "link", "show", "type", "bond").Output()
	if err != nil {
		return nil, nil // No bonds or ip command doesn't support type filter.
	}

	var raw []struct {
		Ifname string `json:"ifname"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, nil
	}

	var bonds []BondConfig
	for _, r := range raw {
		bond := BondConfig{Name: r.Ifname}

		// Get bond mode from sysfs.
		modeOut, _ := exec.Command("cat", "/sys/class/net/"+r.Ifname+"/bonding/mode").Output()
		if mode := strings.TrimSpace(string(modeOut)); mode != "" {
			// Mode is "balance-rr 0" format; take first field.
			parts := strings.Fields(mode)
			if len(parts) > 0 {
				bond.Mode = parts[0]
			}
		}

		// Get bond members from sysfs.
		slavesOut, _ := exec.Command("cat", "/sys/class/net/"+r.Ifname+"/bonding/slaves").Output()
		if slaves := strings.TrimSpace(string(slavesOut)); slaves != "" {
			bond.Members = strings.Fields(slaves)
		}

		// Get IP addresses.
		ipv4, ipv6 := getAddresses(r.Ifname)
		bond.IPv4Addrs = ipv4
		bond.IPv6Addrs = ipv6
		if settings, ok := readNetworkFileSettings(networkDir, r.Ifname); ok {
			bond.applySettings(settings)
		}

		// Get MTU.
		mtuOut, _ := exec.Command("cat", "/sys/class/net/"+r.Ifname+"/mtu").Output()
		if mtu := strings.TrimSpace(string(mtuOut)); mtu != "" {
			fmt.Sscanf(mtu, "%d", &bond.MTU)
		}

		bonds = append(bonds, bond)
	}

	return bonds, nil
}

func (bond *BondConfig) applySettings(settings networkFileSettings) {
	if len(settings.IPv4Addrs) > 0 {
		bond.IPv4Addrs = settings.IPv4Addrs
	}
	if len(settings.IPv6Addrs) > 0 {
		bond.IPv6Addrs = settings.IPv6Addrs
	}
	bond.Gateway4 = settings.Gateway4
	bond.Gateway6 = settings.Gateway6
	bond.DHCP4 = settings.DHCP4
	bond.DHCP6 = settings.DHCP6
	bond.SLAAC = settings.SLAAC
	if settings.MTU > 0 {
		bond.MTU = settings.MTU
	}
	bond.DNS = settings.DNS
}

// ListVLANs discovers VLAN interfaces from the system.
func ListVLANs() ([]VLANConfig, error) {
	return ListVLANsWithConfig(defaultNetworkDir)
}

// ListVLANsWithConfig discovers VLANs and overlays tierd-managed networkd settings.
func ListVLANsWithConfig(networkDir string) ([]VLANConfig, error) {
	out, err := exec.Command("ip", "-j", "link", "show", "type", "vlan").Output()
	if err != nil {
		return nil, nil
	}

	var raw []struct {
		Ifname   string `json:"ifname"`
		Link     string `json:"link"`
		Linkinfo struct {
			InfoData struct {
				ID int `json:"id"`
			} `json:"info_data"`
		} `json:"linkinfo"`
		Mtu int `json:"mtu"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, nil
	}

	var vlans []VLANConfig
	for _, r := range raw {
		vlan := VLANConfig{
			Name:   r.Ifname,
			Parent: r.Link,
			ID:     r.Linkinfo.InfoData.ID,
			MTU:    r.Mtu,
		}

		ipv4, ipv6 := getAddresses(r.Ifname)
		vlan.IPv4Addrs = ipv4
		vlan.IPv6Addrs = ipv6
		if settings, ok := readNetworkFileSettings(networkDir, r.Ifname); ok {
			vlan.applySettings(settings)
		}

		vlans = append(vlans, vlan)
	}

	return vlans, nil
}

func (vlan *VLANConfig) applySettings(settings networkFileSettings) {
	if len(settings.IPv4Addrs) > 0 {
		vlan.IPv4Addrs = settings.IPv4Addrs
	}
	if len(settings.IPv6Addrs) > 0 {
		vlan.IPv6Addrs = settings.IPv6Addrs
	}
	vlan.Gateway4 = settings.Gateway4
	vlan.Gateway6 = settings.Gateway6
	vlan.DHCP4 = settings.DHCP4
	vlan.DHCP6 = settings.DHCP6
	vlan.SLAAC = settings.SLAAC
	if settings.MTU > 0 {
		vlan.MTU = settings.MTU
	}
	vlan.DNS = settings.DNS
}

// ListRoutes returns static routes (non-default, non-link-local) from the system.
func ListRoutes() ([]RouteConfig, error) {
	out, err := exec.Command("ip", "-j", "route", "show").Output()
	if err != nil {
		return nil, nil
	}

	var raw []struct {
		Dst      string `json:"dst"`
		Gateway  string `json:"gateway"`
		Dev      string `json:"dev"`
		Metric   int    `json:"metric"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, nil
	}

	var routes []RouteConfig
	for _, r := range raw {
		// Skip default route, kernel/link-scope routes.
		if r.Dst == "default" || r.Protocol == "kernel" {
			continue
		}
		routes = append(routes, RouteConfig{
			ID:          r.Dst, // Use destination as ID.
			Destination: r.Dst,
			Gateway:     r.Gateway,
			Interface:   r.Dev,
			Metric:      r.Metric,
		})
	}

	return routes, nil
}

// IdentifyInterface blinks the interface LED.
func IdentifyInterface(name string) error {
	if err := ValidateInterfaceName(name); err != nil {
		return err
	}
	cmd := exec.Command("ethtool", "--identify", name, "5")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ethtool identify: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
