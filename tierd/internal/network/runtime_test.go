package network

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRuntimeUplink(t *testing.T) {
	origRun, origResolv := runCmd, resolvConfPath
	t.Cleanup(func() { runCmd, resolvConfPath = origRun, origResolv })

	runCmd = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "ip":
			return []byte(`[{"dst":"default","gateway":"192.168.1.1","dev":"bond0","protocol":"dhcp"}]`), nil
		case "resolvectl":
			return []byte("Link 3 (bond0): 192.168.1.1 fd00::1\n"), nil
		}
		return nil, errors.New("unexpected")
	}
	if gw := RuntimeGateway4("bond0"); gw != "192.168.1.1" {
		t.Errorf("gateway = %q", gw)
	}
	if dns := RuntimeDNS("bond0"); !reflect.DeepEqual(dns, []string{"192.168.1.1", "fd00::1"}) {
		t.Errorf("dns = %v", dns)
	}

	// Without systemd-resolved, fall back to resolv.conf, skipping the stub.
	resolvConfPath = filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(resolvConfPath, []byte("nameserver 127.0.0.53\nnameserver 192.168.1.1\nsearch lan\n"), 0644)
	runCmd = func(string, ...string) ([]byte, error) { return nil, errors.New("not found") }
	if dns := RuntimeDNS("bond0"); !reflect.DeepEqual(dns, []string{"192.168.1.1"}) {
		t.Errorf("fallback dns = %v", dns)
	}
	if gw := RuntimeGateway4("bond0"); gw != "" {
		t.Errorf("gateway without ip = %q", gw)
	}
	if RuntimeGateway4("bad name;rm") != "" || RuntimeDNS("bad name;rm") != nil {
		t.Error("invalid interface names must be rejected")
	}
}
