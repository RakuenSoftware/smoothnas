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
	origDev, origGW, origDNS := defaultRouteDevice, runtimeGateway4, runtimeDNS
	t.Cleanup(func() { defaultRouteDevice, runtimeGateway4, runtimeDNS = origDev, origGW, origDNS })
	defaultRouteDevice = func() string { return "bond0" }
	leaseGW, leaseDNS := "192.168.1.1", []string{"192.168.1.1"}
	runtimeGateway4 = func(string) string { return leaseGW }
	runtimeDNS = func(string) []string { return leaseDNS }

	// DHCP -> static on the uplink with gateway and DNS left empty keeps
	// the ones the lease provided.
	gw, dns := "", []string(nil)
	rec := httptest.NewRecorder()
	if !requireUplinkConfig(rec, "bond0", false, []string{"192.168.1.254/23"}, &gw, &dns) {
		t.Fatalf("refused: %s", rec.Body.String())
	}
	if gw != "192.168.1.1" || len(dns) != 1 || dns[0] != "192.168.1.1" {
		t.Fatalf("expected lease values kept, got gw=%q dns=%v", gw, dns)
	}

	// Values the operator entered win over the lease.
	gw, dns = "192.168.1.2", []string{"9.9.9.9"}
	if !requireUplinkConfig(httptest.NewRecorder(), "bond0", false, []string{"192.168.1.254/23"}, &gw, &dns) || gw != "192.168.1.2" || dns[0] != "9.9.9.9" {
		t.Fatalf("operator values overwritten: gw=%q dns=%v", gw, dns)
	}

	// Nothing to keep (no lease DNS): refused with a coded error.
	leaseDNS = nil
	gw, dns = "", nil
	rec = httptest.NewRecorder()
	if requireUplinkConfig(rec, "bond0", false, []string{"192.168.1.254/23"}, &gw, &dns) {
		t.Fatal("expected refusal without any DNS server")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "network.uplink_needs_gateway_dns") {
		t.Fatalf("expected a coded 400, got %d %s", rec.Code, rec.Body.String())
	}

	// A storage-only NIC may be static without either.
	gw, dns = "", nil
	if !requireUplinkConfig(httptest.NewRecorder(), "enp2s0", false, []string{"10.10.0.2/24"}, &gw, &dns) || gw != "" || dns != nil {
		t.Fatalf("storage NIC changed or refused: gw=%q dns=%v", gw, dns)
	}
}
