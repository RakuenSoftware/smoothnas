package network

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// DNSConfig holds DNS and hostname settings.
type DNSConfig struct {
	Servers       []string `json:"servers"`
	SearchDomains []string `json:"search_domains"`
}

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
	if err := ValidateIPv4(server); err == nil {
		return nil
	}
	// Allow IPv6.
	if strings.Contains(server, ":") {
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

// GetDNS reads current DNS config from resolved or resolv.conf.
func GetDNS() (*DNSConfig, error) {
	out, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return &DNSConfig{}, nil
	}

	config := &DNSConfig{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "nameserver ") {
			config.Servers = append(config.Servers, strings.TrimPrefix(line, "nameserver "))
		}
		if strings.HasPrefix(line, "search ") {
			config.SearchDomains = strings.Fields(strings.TrimPrefix(line, "search "))
		}
	}
	return config, nil
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
