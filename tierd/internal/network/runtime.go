package network

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
)

// Runtime (as opposed to configured) uplink settings. On DHCP the gateway
// and DNS servers come from the lease, so the .network file holds none;
// these report what the link is actually using, so a switch to a static
// address can keep them instead of dropping them.

// runCmd is a var so tests can fake command output.
var runCmd = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// resolvConfPath is the fallback DNS source when systemd-resolved is absent.
var resolvConfPath = "/etc/resolv.conf"

// RuntimeGateway4 returns the IPv4 default gateway in use on dev, or "".
func RuntimeGateway4(dev string) string {
	if ValidateInterfaceName(dev) != nil {
		return ""
	}
	out, err := runCmd("ip", "-4", "-j", "route", "show", "default", "dev", dev)
	if err != nil {
		return ""
	}
	var routes []struct {
		Gateway string `json:"gateway"`
	}
	if json.Unmarshal(out, &routes) != nil {
		return ""
	}
	for _, r := range routes {
		if r.Gateway != "" {
			return r.Gateway
		}
	}
	return ""
}

// RuntimeDNS returns the DNS servers in use on dev: the link's servers from
// systemd-resolved, else the nameservers in /etc/resolv.conf.
func RuntimeDNS(dev string) []string {
	if ValidateInterfaceName(dev) != nil {
		return nil
	}
	if out, err := runCmd("resolvectl", "dns", dev); err == nil {
		// "Link 2 (bond0): 192.168.1.1 fd00::1"
		if i := strings.Index(string(out), "):"); i >= 0 {
			if servers := strings.Fields(string(out)[i+2:]); len(servers) > 0 {
				return servers
			}
		}
	}
	data, err := os.ReadFile(resolvConfPath)
	if err != nil {
		return nil
	}
	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		// 127.0.0.53 is resolved's stub, not an upstream server.
		if len(f) >= 2 && f[0] == "nameserver" && f[1] != "127.0.0.53" {
			servers = append(servers, f[1])
		}
	}
	return servers
}
