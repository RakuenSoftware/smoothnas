// Package network manages network configuration via systemd-networkd.
//
// tierd owns all files in /etc/systemd/network/. It generates .network,
// .netdev, and .link files from its internal state. Manual edits are
// overwritten. Changes are applied via networkctl reload.
package network

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Interface represents a physical network interface.
type Interface struct {
	Name      string   `json:"name"`       // e.g. "eth0", "enp3s0"
	MAC       string   `json:"mac"`
	State     string   `json:"state"`      // "up", "down", "unknown"
	Speed     string   `json:"speed"`      // e.g. "1000Mbps"
	MTU       int      `json:"mtu"`
	Driver    string   `json:"driver"`
	IPv4Addrs []string `json:"ipv4_addrs"` // CIDR notation
	IPv6Addrs []string `json:"ipv6_addrs"` // CIDR notation
	Gateway4  string   `json:"gateway4"`
	Gateway6  string   `json:"gateway6"`
	DHCP4     bool     `json:"dhcp4"`
	DHCP6     bool     `json:"dhcp6"`
	SLAAC     bool     `json:"slaac"`      // IPv6 SLAAC (accept RA)
	Assignment string  `json:"assignment"` // "standalone", "bond-member", "unused"
	BondName  string   `json:"bond_name"`  // non-empty if bond member
}

var ifaceNameRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._-]{0,15}$`)

// ValidateInterfaceName checks that an interface name is safe.
func ValidateInterfaceName(name string) error {
	if !ifaceNameRegex.MatchString(name) {
		return fmt.Errorf("invalid interface name: %s", name)
	}
	return nil
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
	out, err := exec.Command("ip", "-j", "link", "show").Output()
	if err != nil {
		return nil, fmt.Errorf("ip link show: %w", err)
	}

	var raw []struct {
		Ifname   string `json:"ifname"`
		Address  string `json:"address"`
		Operstate string `json:"operstate"`
		Mtu      int    `json:"mtu"`
		LinkType string `json:"link_type"`
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

		iface := Interface{
			Name:       r.Ifname,
			MAC:        r.Address,
			State:      r.Operstate,
			MTU:        r.Mtu,
			Assignment: "standalone",
		}

		// Get IP addresses.
		iface.IPv4Addrs, iface.IPv6Addrs = getAddresses(r.Ifname)

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

	if iface.DHCP4 {
		b.WriteString("DHCP=ipv4\n")
	}
	if iface.DHCP6 {
		b.WriteString("DHCP=ipv6\n")
	}
	if iface.DHCP4 && iface.DHCP6 {
		// Override: both
		b.WriteString("DHCP=yes\n")
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
			os.Remove(filepath.Join(networkDir, e.Name()))
		}
	}
	return nil
}

// ListBonds discovers bond interfaces from the system.
func ListBonds() ([]BondConfig, error) {
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

		// Get MTU.
		mtuOut, _ := exec.Command("cat", "/sys/class/net/"+r.Ifname+"/mtu").Output()
		if mtu := strings.TrimSpace(string(mtuOut)); mtu != "" {
			fmt.Sscanf(mtu, "%d", &bond.MTU)
		}

		bonds = append(bonds, bond)
	}

	return bonds, nil
}

// ListVLANs discovers VLAN interfaces from the system.
func ListVLANs() ([]VLANConfig, error) {
	out, err := exec.Command("ip", "-j", "link", "show", "type", "vlan").Output()
	if err != nil {
		return nil, nil
	}

	var raw []struct {
		Ifname  string `json:"ifname"`
		Link    string `json:"link"`
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

		vlans = append(vlans, vlan)
	}

	return vlans, nil
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
