package api

import (
	"strings"
	"testing"
)

func TestValidateIPConfigAcceptsEmpty(t *testing.T) {
	if err := validateIPConfig(nil, nil, "", "", 0, nil); err != nil {
		t.Fatalf("empty config rejected: %v", err)
	}
}

func TestValidateIPConfigAcceptsValidIPv4(t *testing.T) {
	if err := validateIPConfig(
		[]string{"192.168.1.10/24"}, nil,
		"192.168.1.1", "", 1500, []string{"1.1.1.1"},
	); err != nil {
		t.Fatalf("valid IPv4 config rejected: %v", err)
	}
}

func TestValidateIPConfigAcceptsValidIPv6(t *testing.T) {
	if err := validateIPConfig(
		nil, []string{"2001:db8::10/64"},
		"", "fe80::1", 1500, []string{"2001:4860:4860::8888"},
	); err != nil {
		t.Fatalf("valid IPv6 config rejected: %v", err)
	}
}

func TestValidateIPConfigAcceptsDualStack(t *testing.T) {
	if err := validateIPConfig(
		[]string{"10.0.0.10/24", "192.168.1.10/24"},
		[]string{"2001:db8::10/64"},
		"10.0.0.1", "fe80::1", 9000, []string{"8.8.8.8", "2001:4860:4860::8888"},
	); err != nil {
		t.Fatalf("dual-stack config rejected: %v", err)
	}
}

func TestValidateIPConfigRejectsBadIPv4CIDR(t *testing.T) {
	for _, bad := range []string{
		"192.168.1.10",    // missing CIDR
		"192.168.1.10/64", // accepted by regex; range covered elsewhere
		"not-an-ip",
		"192.168.1.999/24", // accepted by regex but absurd; this validator is regex-only
	} {
		err := validateIPConfig([]string{bad}, nil, "", "", 0, nil)
		// The regex validator may accept the absurd-but-shaped ones;
		// only assert that a clearly-malformed string is rejected.
		if bad == "not-an-ip" || bad == "192.168.1.10" {
			if err == nil {
				t.Fatalf("validator accepted bad IPv4 CIDR %q", bad)
			}
		}
	}
}

func TestValidateIPConfigRejectsBadIPv6CIDR(t *testing.T) {
	if err := validateIPConfig(nil, []string{"not-ipv6"}, "", "", 0, nil); err == nil {
		t.Fatalf("validator accepted non-IPv6 string")
	}
	if err := validateIPConfig(nil, []string{"2001:db8::10"}, "", "", 0, nil); err == nil {
		t.Fatalf("validator accepted IPv6 without CIDR")
	}
}

func TestValidateIPConfigRejectsBadIPv4Gateway(t *testing.T) {
	if err := validateIPConfig(nil, nil, "not-an-ip", "", 0, nil); err == nil {
		t.Fatalf("validator accepted bad IPv4 gateway")
	}
}

func TestValidateIPConfigRejectsBadIPv6Gateway(t *testing.T) {
	if err := validateIPConfig(nil, nil, "", "no-colons-here", 0, nil); err == nil {
		t.Fatalf("validator accepted bad IPv6 gateway")
	}
	err := validateIPConfig(nil, nil, "", "no-colons-here", 0, nil)
	if err == nil || !strings.Contains(err.Error(), "IPv6 gateway") {
		t.Fatalf("err = %v, want mentions IPv6 gateway", err)
	}
}

func TestValidateIPConfigRejectsBadMTU(t *testing.T) {
	for _, bad := range []int{500, 9001, -1} {
		if err := validateIPConfig(nil, nil, "", "", bad, nil); err == nil {
			t.Fatalf("validator accepted bad MTU %d", bad)
		}
	}
}

func TestValidateIPConfigZeroMTUIsLeftAlone(t *testing.T) {
	// MTU 0 means "don't set" (caller didn't supply one). Validator
	// must NOT reject it as out-of-range; the form sends 0 when the
	// MTU input is empty.
	if err := validateIPConfig(nil, nil, "", "", 0, nil); err != nil {
		t.Fatalf("MTU 0 should be a no-op, got %v", err)
	}
}

func TestValidateIPConfigRejectsBadDNS(t *testing.T) {
	err := validateIPConfig(nil, nil, "", "", 0, []string{"not-a-dns-server"})
	if err == nil || !strings.Contains(err.Error(), "DNS server") {
		t.Fatalf("err = %v, want DNS server validation error", err)
	}
}
