package compose

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// HostPort is a fixed host-published port parsed from a compose ports: entry.
type HostPort struct {
	Port     int
	Protocol string // "tcp" (default) or "udp"
}

// Key is the canonical collision key (port + protocol).
func (h HostPort) Key() string { return fmt.Sprintf("%d/%s", h.Port, h.Protocol) }

// HostPorts parses the FIXED host-published ports from a compose project's
// services.*.ports — the ones that can collide with another project on the host.
// Handles the short forms ("8080:80", "8080:80/udp", "127.0.0.1:8080:80") and
// the long form ({published, target, protocol}). Container-only entries ("80")
// and ephemeral publishes (no published port) are ignored — the runtime picks a
// free host port, so they can't statically collide. Port ranges are skipped
// (best-effort; a range collision surfaces at runtime).
func HostPorts(composeYAML []byte) ([]HostPort, error) {
	var doc struct {
		Services map[string]struct {
			Ports []yaml.Node `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse ports: %w", err)
	}
	var out []HostPort
	for _, svc := range doc.Services {
		for _, n := range svc.Ports {
			hp, ok := parsePortNode(n)
			if ok {
				out = append(out, hp)
			}
		}
	}
	return out, nil
}

func parsePortNode(n yaml.Node) (HostPort, bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		return parsePortString(n.Value)
	case yaml.MappingNode:
		var m struct {
			Published any    `yaml:"published"`
			Protocol  string `yaml:"protocol"`
		}
		if err := n.Decode(&m); err != nil {
			return HostPort{}, false
		}
		p, ok := toInt(m.Published)
		if !ok || p == 0 {
			return HostPort{}, false // ephemeral / unparseable
		}
		return HostPort{Port: p, Protocol: proto(m.Protocol)}, true
	}
	return HostPort{}, false
}

// parsePortString handles "CONTAINER", "HOST:CONTAINER", "IP:HOST:CONTAINER",
// each optionally suffixed "/tcp" or "/udp".
func parsePortString(s string) (HostPort, bool) {
	s = strings.TrimSpace(s)
	protocol := "tcp"
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		protocol = proto(s[i+1:])
		s = s[:i]
	}
	// Strip a bracketed IPv6 host-IP prefix ("[::1]:8080:80") so the ':' split
	// below sees only host:container. NOTE: the guard keys on port/proto only
	// (not host_ip) — consistent with the manifest hostExpose guard and the
	// single-host DNAT model, so same-port-on-distinct-host-IPs is treated as a
	// conflict by policy.
	if strings.HasPrefix(s, "[") {
		if j := strings.IndexByte(s, ']'); j >= 0 {
			s = strings.TrimPrefix(s[j+1:], ":")
		}
	}
	parts := strings.Split(s, ":")
	var hostPart string
	switch len(parts) {
	case 1:
		return HostPort{}, false // container-only, no host publish
	case 2:
		hostPart = parts[0] // HOST:CONTAINER
	case 3:
		hostPart = parts[1] // IP:HOST:CONTAINER
	default:
		return HostPort{}, false
	}
	if strings.ContainsRune(hostPart, '-') { // range, skip
		return HostPort{}, false
	}
	p, err := strconv.Atoi(strings.TrimSpace(hostPart))
	if err != nil || p == 0 {
		return HostPort{}, false
	}
	return HostPort{Port: p, Protocol: protocol}, true
}

// proto normalizes the transport to compose's supported set (tcp/udp/sctp),
// preserving sctp rather than collapsing it to tcp (which would false-match).
func proto(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "udp":
		return "udp"
	case "sctp":
		return "sctp"
	default:
		return "tcp"
	}
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case string:
		if strings.ContainsRune(t, '-') {
			return 0, false // range
		}
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}
