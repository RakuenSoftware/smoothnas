package firewall

import (
	"strings"
	"testing"
)

func TestValidateProtocol(t *testing.T) {
	valid := []string{"smb", "nfs", "iscsi"}
	for _, proto := range valid {
		if err := ValidateProtocol(proto); err != nil {
			t.Errorf("expected valid protocol %q, got error: %v", proto, err)
		}
	}

	invalid := []string{"", "http", "ssh", "ftp", "cifs"}
	for _, proto := range invalid {
		if err := ValidateProtocol(proto); err == nil {
			t.Errorf("expected invalid protocol %q to fail", proto)
		}
	}
}

func TestGenerateRulesetNoProtocols(t *testing.T) {
	ruleset := GenerateRuleset(map[string]bool{})

	// Should contain base rules.
	if !strings.Contains(ruleset, "tcp dport 22 accept") {
		t.Error("missing SSH rule")
	}
	if !strings.Contains(ruleset, "tcp dport 80 accept") {
		t.Error("missing HTTP redirect rule")
	}
	if !strings.Contains(ruleset, "tcp dport 443 accept") {
		t.Error("missing HTTPS rule")
	}

	// Should NOT contain any sharing protocol ports.
	if strings.Contains(ruleset, "dport 445") {
		t.Error("SMB port should not be present when disabled")
	}
	if strings.Contains(ruleset, "dport 2049") {
		t.Error("NFS port should not be present when disabled")
	}
	if strings.Contains(ruleset, "dport 3260") {
		t.Error("iSCSI port should not be present when disabled")
	}
}

func TestGenerateRulesetSMBOnly(t *testing.T) {
	ruleset := GenerateRuleset(map[string]bool{"smb": true})

	if !strings.Contains(ruleset, "dport 445 accept") {
		t.Error("missing SMB port 445")
	}
	if strings.Contains(ruleset, "dport 2049") {
		t.Error("NFS port should not be present")
	}
	if strings.Contains(ruleset, "dport 3260") {
		t.Error("iSCSI port should not be present")
	}
}

func TestGenerateRulesetNFSOnly(t *testing.T) {
	ruleset := GenerateRuleset(map[string]bool{"nfs": true})

	if !strings.Contains(ruleset, "dport 2049 accept") {
		t.Error("missing NFS port 2049")
	}
	if !strings.Contains(ruleset, "dport 111 accept") {
		t.Error("missing rpcbind port 111")
	}
	// Should have both TCP and UDP for rpcbind.
	if !strings.Contains(ruleset, "tcp dport 111") {
		t.Error("missing TCP rpcbind")
	}
	if !strings.Contains(ruleset, "udp dport 111") {
		t.Error("missing UDP rpcbind")
	}
	for _, port := range []string{"20048", "32765", "32767"} {
		if !strings.Contains(ruleset, "tcp dport "+port) {
			t.Errorf("missing TCP NFS helper port %s", port)
		}
		if !strings.Contains(ruleset, "udp dport "+port) {
			t.Errorf("missing UDP NFS helper port %s", port)
		}
	}
}

func TestGenerateRulesetISCSIOnly(t *testing.T) {
	ruleset := GenerateRuleset(map[string]bool{"iscsi": true})

	if !strings.Contains(ruleset, "dport 3260 accept") {
		t.Error("missing iSCSI port 3260")
	}
}

func TestGenerateRulesetAllProtocols(t *testing.T) {
	ruleset := GenerateRuleset(map[string]bool{
		"smb": true, "nfs": true, "iscsi": true,
	})

	if !strings.Contains(ruleset, "dport 445") {
		t.Error("missing SMB")
	}
	if !strings.Contains(ruleset, "dport 2049") {
		t.Error("missing NFS")
	}
	if !strings.Contains(ruleset, "dport 3260") {
		t.Error("missing iSCSI")
	}
}

func TestGenerateRulesetStructure(t *testing.T) {
	ruleset := GenerateRuleset(map[string]bool{})

	// Should have proper nftables structure. tierd must scope its apply
	// to its own table (not `flush ruleset`) so the plugin runtime's
	// nat/filter tables — including the veth_nat DNAT rules that publish
	// hostExpose ports — survive a firewall re-apply.
	if strings.Contains(ruleset, "flush ruleset") {
		t.Error("must not flush the whole ruleset; that wipes the runtime's DNAT table")
	}
	if !strings.Contains(ruleset, "delete table inet filter") {
		t.Error("missing scoped replace of table inet filter")
	}
	if !strings.Contains(ruleset, "table inet filter") {
		t.Error("missing table inet filter")
	}
	if !strings.Contains(ruleset, "chain input") {
		t.Error("missing input chain")
	}
	if !strings.Contains(ruleset, "chain forward") {
		t.Error("missing forward chain")
	}
	if !strings.Contains(ruleset, "chain output") {
		t.Error("missing output chain")
	}
	if !strings.Contains(ruleset, "policy drop") {
		t.Error("missing default drop policy on input")
	}
	if !strings.Contains(ruleset, "ct state established,related accept") {
		t.Error("missing established/related rule")
	}
	if !strings.Contains(ruleset, `iifname "veth0" accept`) {
		t.Error("missing plugin bridge egress rule")
	}
	if !strings.Contains(ruleset, `ct state established,related oifname "veth0" accept`) {
		t.Error("missing plugin bridge return-traffic rule")
	}
	if !strings.Contains(ruleset, `ct status dnat oifname "veth0" accept`) {
		t.Error("missing inbound rule for DNAT'd hostExpose plugin ports")
	}
	if !strings.Contains(ruleset, "iif lo accept") {
		t.Error("missing loopback rule")
	}
}

func TestProtocolPortsCompleteness(t *testing.T) {
	// Ensure all known protocols have port definitions.
	for _, proto := range []string{"smb", "nfs", "iscsi"} {
		ports, ok := ProtocolPorts[proto]
		if !ok {
			t.Errorf("missing port definition for protocol %s", proto)
		}
		if len(ports) == 0 {
			t.Errorf("empty port list for protocol %s", proto)
		}
	}
}
