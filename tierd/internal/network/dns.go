package network

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// DNSConfig holds DNS and hostname settings.
type DNSConfig struct {
	Servers       []string `json:"servers"`
	SearchDomains []string `json:"search_domains"`
}

const (
	defaultResolvedConfDir      = "/etc/systemd/resolved.conf.d"
	smoothNASResolvedConfigFile = "smoothnas.conf"
)

// RouteConfig holds a static route.
type RouteConfig struct {
	ID          string `json:"id"`          // generated
	Destination string `json:"destination"` // CIDR, e.g. "10.100.0.0/16"
	Gateway     string `json:"gateway"`
	Interface   string `json:"interface"`
	Metric      int    `json:"metric"`
}

var hostnameRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,62}$`)
var domainRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]+$`)

// ValidateHostname checks that a hostname is safe.
func ValidateHostname(name string) error {
	if !hostnameRegex.MatchString(name) {
		return fmt.Errorf("invalid hostname: %s (must start with letter, alphanumeric/hyphens, max 63 chars)", name)
	}
	return nil
}

// ValidateDNSServer checks that a DNS server is a valid IP.
func ValidateDNSServer(server string) error {
	if net.ParseIP(server) != nil {
		return nil
	}
	return fmt.Errorf("invalid DNS server: %s", server)
}

// ValidateSearchDomain checks that a search domain is safe.
func ValidateSearchDomain(domain string) error {
	if !domainRegex.MatchString(domain) {
		return fmt.Errorf("invalid search domain: %s", domain)
	}
	return nil
}

// ValidateRouteCIDR checks that a route destination is valid CIDR.
func ValidateRouteCIDR(cidr string) error {
	if err := ValidateIPv4CIDR(cidr); err == nil {
		return nil
	}
	if err := ValidateIPv6CIDR(cidr); err == nil {
		return nil
	}
	return fmt.Errorf("invalid route destination: %s", cidr)
}

// GetHostname returns the current hostname.
func GetHostname() (string, error) {
	out, err := exec.Command("hostname").Output()
	if err != nil {
		return "", fmt.Errorf("hostname: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// SetHostname changes the system hostname.
func SetHostname(name string) error {
	if err := ValidateHostname(name); err != nil {
		return err
	}

	cmd := exec.Command("hostnamectl", "set-hostname", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("hostnamectl: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// GetDNS reads current upstream DNS config from systemd-resolved.
//
// /etc/resolv.conf usually points at the local resolved stub
// (127.0.0.53), so using it as the primary source reports the
// proxy instead of the operator's configured upstream servers.
func GetDNS() (*DNSConfig, error) {
	return getDNSWithRunner(func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}, []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"})
}

func getDNSWithRunner(run commandRunner, resolvConfPaths []string) (*DNSConfig, error) {
	config := &DNSConfig{}

	if out, err := run("resolvectl", "dns"); err == nil {
		config.Servers = parseResolvectlDNS(string(out))
	}
	if out, err := run("resolvectl", "domain"); err == nil {
		config.SearchDomains = parseResolvectlDomains(string(out))
	}

	if len(config.Servers) == 0 || len(config.SearchDomains) == 0 {
		fallback := readResolvConfFallback(resolvConfPaths)
		if len(config.Servers) == 0 {
			config.Servers = fallback.Servers
		}
		if len(config.SearchDomains) == 0 {
			config.SearchDomains = fallback.SearchDomains
		}
	}

	return config, nil
}

func parseResolvectlDNS(out string) []string {
	var servers []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		for _, token := range strings.Fields(rest) {
			server := normalizeDNSToken(token)
			if server == "" || seen[server] || isLocalResolver(server) {
				continue
			}
			if ValidateDNSServer(server) != nil {
				continue
			}
			seen[server] = true
			servers = append(servers, server)
		}
	}
	return servers
}

func parseResolvectlDomains(out string) []string {
	var domains []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		for _, token := range strings.Fields(rest) {
			domain := strings.TrimSpace(token)
			if domain == "" || domain == "." || strings.HasPrefix(domain, "~") || seen[domain] {
				continue
			}
			if ValidateSearchDomain(domain) != nil {
				continue
			}
			seen[domain] = true
			domains = append(domains, domain)
		}
	}
	return domains
}

func readResolvConfFallback(paths []string) DNSConfig {
	config := DNSConfig{}
	seenServers := map[string]bool{}
	seenDomains := map[string]bool{}
	for _, path := range paths {
		out, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver ") {
				server := normalizeDNSToken(strings.TrimSpace(strings.TrimPrefix(line, "nameserver ")))
				if server != "" && !isLocalResolver(server) && !seenServers[server] && ValidateDNSServer(server) == nil {
					seenServers[server] = true
					config.Servers = append(config.Servers, server)
				}
			}
			if strings.HasPrefix(line, "search ") {
				for _, domain := range strings.Fields(strings.TrimPrefix(line, "search ")) {
					if domain == "." || strings.HasPrefix(domain, "~") || seenDomains[domain] || ValidateSearchDomain(domain) != nil {
						continue
					}
					seenDomains[domain] = true
					config.SearchDomains = append(config.SearchDomains, domain)
				}
			}
		}
		if len(config.Servers) > 0 || len(config.SearchDomains) > 0 {
			break
		}
	}
	return config
}

func normalizeDNSToken(token string) string {
	token = strings.TrimSpace(token)
	token = strings.Trim(token, "[]")
	if before, _, ok := strings.Cut(token, "#"); ok {
		token = before
	}
	if before, _, ok := strings.Cut(token, "%"); ok {
		token = before
	}
	return token
}

func isLocalResolver(server string) bool {
	ip := net.ParseIP(server)
	return ip != nil && ip.IsLoopback()
}

// SetDNS writes global systemd-resolved DNS settings.
func SetDNS(config DNSConfig) error {
	return setDNSWithRunner(config, defaultResolvedConfDir, func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	})
}

func setDNSWithRunner(config DNSConfig, confDir string, run commandRunner) error {
	for _, s := range config.Servers {
		if err := ValidateDNSServer(s); err != nil {
			return err
		}
	}
	for _, d := range config.SearchDomains {
		if err := ValidateSearchDomain(d); err != nil {
			return err
		}
	}

	path := filepath.Join(confDir, smoothNASResolvedConfigFile)
	if len(config.Servers) == 0 && len(config.SearchDomains) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		if err := os.MkdirAll(confDir, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(GenerateResolvedDNSDropIn(config)), 0644); err != nil {
			return err
		}
	}

	if out, err := run("systemctl", "restart", "systemd-resolved"); err != nil {
		return fmt.Errorf("restart systemd-resolved: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// GenerateResolvedDNSDropIn generates the systemd-resolved drop-in managed by tierd.
func GenerateResolvedDNSDropIn(config DNSConfig) string {
	var b strings.Builder
	b.WriteString("# Auto-generated by tierd. Do not edit.\n")
	b.WriteString("[Resolve]\n")
	if len(config.Servers) > 0 {
		fmt.Fprintf(&b, "DNS=%s\n", strings.Join(config.Servers, " "))
	}
	if len(config.SearchDomains) > 0 {
		fmt.Fprintf(&b, "Domains=%s\n", strings.Join(config.SearchDomains, " "))
	}
	return b.String()
}

// GenerateRouteSection generates [Route] sections for a .network file.
func GenerateRouteSection(routes []RouteConfig) string {
	var b strings.Builder
	for _, r := range routes {
		b.WriteString("\n[Route]\n")
		fmt.Fprintf(&b, "Destination=%s\n", r.Destination)
		if r.Gateway != "" {
			fmt.Fprintf(&b, "Gateway=%s\n", r.Gateway)
		}
		if r.Metric > 0 {
			fmt.Fprintf(&b, "Metric=%d\n", r.Metric)
		}
	}
	return b.String()
}
