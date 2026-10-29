package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNetworkCreateBondRejectsRuntimeInterfaces(t *testing.T) {
	h := NewNetworkHandler(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/network/bonds", strings.NewReader(`{
		"name":"bond0",
		"mode":"balance-alb",
		"members":["enp1s0","veth1234"]
	}`))
	w := httptest.NewRecorder()

	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot be used as a bond member") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestNetworkUpdateBondRejectsRuntimeInterfaces(t *testing.T) {
	h := NewNetworkHandler(nil)
	req := httptest.NewRequest(http.MethodPut, "/api/network/bonds/bond0", strings.NewReader(`{
		"mode":"balance-alb",
		"members":["gow0"]
	}`))
	w := httptest.NewRecorder()

	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot be used as a bond member") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// TestNetworkCreateBondRejectsEmptyMembers ensures the API refuses to create a
// bond with no member interfaces. Such a bond can never gain carrier — it comes
// up DOWN and strands the IP config written to it. On a real appliance an
// edit-IP request with an empty member list wrote a static IP onto bond0 and
// left it permanently down; this guard turns that into a clear 400.
func TestNetworkCreateBondRejectsEmptyMembers(t *testing.T) {
	h := NewNetworkHandler(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/network/bonds", strings.NewReader(`{
		"name":"bond0",
		"mode":"balance-alb",
		"members":[],
		"ipv4_addrs":["192.168.1.254/23"],
		"gateway4":"192.168.1.1"
	}`))
	w := httptest.NewRecorder()

	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "at least one member") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestNetworkUpdateBondRejectsEmptyMembers(t *testing.T) {
	h := NewNetworkHandler(nil)
	req := httptest.NewRequest(http.MethodPut, "/api/network/bonds/bond0", strings.NewReader(`{
		"mode":"balance-alb",
		"members":[],
		"ipv4_addrs":["192.168.1.254/23"],
		"gateway4":"192.168.1.1"
	}`))
	w := httptest.NewRecorder()

	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "at least one member") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestRequireUplinkConfig(t *testing.T) {
	orig := defaultRouteDevice
	defaultRouteDevice = func() string { return "bond0" }
	t.Cleanup(func() { defaultRouteDevice = orig })

	cases := []struct {
		name    string
		iface   string
		dhcp    bool
		ipv4    []string
		gw      string
		dns     []string
		allowed bool
	}{
		{"static uplink without DNS", "bond0", false, []string{"192.168.1.254/23"}, "192.168.1.1", nil, false},
		{"static uplink without gateway", "bond0", false, []string{"192.168.1.254/23"}, "", []string{"192.168.1.1"}, false},
		{"static uplink complete", "bond0", false, []string{"192.168.1.254/23"}, "192.168.1.1", []string{"192.168.1.1"}, true},
		{"DHCP uplink", "bond0", true, nil, "", nil, true},
		{"static storage-only NIC", "enp2s0", false, []string{"10.10.0.2/24"}, "", nil, true},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		got := requireUplinkConfig(rec, c.iface, c.dhcp, c.ipv4, c.gw, c.dns)
		if got != c.allowed {
			t.Errorf("%s: allowed=%v, want %v (body %s)", c.name, got, c.allowed, rec.Body.String())
		}
		if !got && (rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "network.uplink_needs_gateway_dns")) {
			t.Errorf("%s: expected a coded 400, got %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
}
