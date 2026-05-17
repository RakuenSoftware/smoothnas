package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestPluginBridgeNameUsesRuntimeBridgeName(t *testing.T) {
	if PluginBridgeName != "veth" {
		t.Fatalf("PluginBridgeName = %q, want veth", PluginBridgeName)
	}
	if PluginBridgeIface != "veth0" {
		t.Fatalf("PluginBridgeIface = %q, want veth0", PluginBridgeIface)
	}
}

func TestEnsurePluginBridge_CreatesWhenMissing(t *testing.T) {
	var seenCreate CreateNetworkRequest
	createCalls := 0
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/networks/"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
		case r.Method == http.MethodPost && r.URL.Path == "/networks/create":
			createCalls++
			_ = json.NewDecoder(r.Body).Decode(&seenCreate)
			_ = json.NewEncoder(w).Encode(CreateNetworkResponse{ID: "net-1"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	c := NewClient(sock)
	id, err := c.EnsurePluginBridge(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if id != "net-1" {
		t.Errorf("id = %q want net-1", id)
	}
	if createCalls != 1 {
		t.Errorf("create calls = %d want 1", createCalls)
	}
	if seenCreate.Name != PluginBridgeName {
		t.Errorf("Name = %q", seenCreate.Name)
	}
	if seenCreate.Driver != "bridge" {
		t.Errorf("Driver = %q", seenCreate.Driver)
	}
	if len(seenCreate.IPAM.Config) != 1 || seenCreate.IPAM.Config[0].Subnet != PluginBridgeSubnet {
		t.Errorf("IPAM = %+v", seenCreate.IPAM)
	}
	if seenCreate.Labels[PluginManagedLabel] != "true" {
		t.Errorf("managed label missing: %+v", seenCreate.Labels)
	}
}

func TestEnsurePluginBridge_ReusesExistingMatchingSubnet(t *testing.T) {
	createCalls := 0
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/networks/"):
			_ = json.NewEncoder(w).Encode(NetworkInspect{
				ID:   "existing-1",
				Name: PluginBridgeName,
				IPAM: NetworkIPAM{Config: []NetworkIPAMRange{{Subnet: PluginBridgeSubnet}}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/networks/create":
			createCalls++
			_ = json.NewEncoder(w).Encode(CreateNetworkResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	c := NewClient(sock)
	id, err := c.EnsurePluginBridge(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if id != "existing-1" {
		t.Errorf("id = %q want existing-1", id)
	}
	if createCalls != 0 {
		t.Errorf("create should not have been called; got %d", createCalls)
	}
}

func TestEnsurePluginBridge_RejectsExistingWithWrongSubnet(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(NetworkInspect{
			ID:   "existing-2",
			Name: PluginBridgeName,
			IPAM: NetworkIPAM{Config: []NetworkIPAMRange{{Subnet: "172.99.0.0/16"}}},
		})
	}))
	c := NewClient(sock)
	_, err := c.EnsurePluginBridge(context.Background())
	if err == nil {
		t.Fatal("expected error for unexpected subnet")
	}
	if !strings.Contains(err.Error(), "unexpected subnet") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsurePluginBridge_HandlesCreateRace(t *testing.T) {
	// Simulate the race: inspect 404s, create returns 409 (someone
	// else just created it), re-inspect succeeds.
	step := 0
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && step == 0:
			step = 1
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
		case r.Method == http.MethodPost && r.URL.Path == "/networks/create":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "name already in use"})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(NetworkInspect{
				ID:   "raced-net",
				Name: PluginBridgeName,
				IPAM: NetworkIPAM{Config: []NetworkIPAMRange{{Subnet: PluginBridgeSubnet}}},
			})
		}
	}))
	c := NewClient(sock)
	id, err := c.EnsurePluginBridge(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if id != "raced-net" {
		t.Errorf("id = %q want raced-net", id)
	}
}

func TestInspectContainerBridgeIP_ReturnsErrNotReadyOnEmptyAddress(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ContainerInspect{
			ID: "abc",
			NetworkSettings: ContainerNetworkSettings{
				Networks: map[string]ContainerNetwork{
					PluginBridgeName: {IPAddress: ""},
				},
			},
		})
	}))
	c := NewClient(sock)
	_, err := c.InspectContainerBridgeIP(context.Background(), "abc")
	if !errors.Is(err, ErrBridgeIPNotReady) {
		t.Errorf("err = %v want ErrBridgeIPNotReady", err)
	}
}

func TestInspectContainerBridgeIP_ReturnsAddressWhenReady(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ContainerInspect{
			ID: "abc",
			NetworkSettings: ContainerNetworkSettings{
				Networks: map[string]ContainerNetwork{
					PluginBridgeName: {IPAddress: "10.100.0.42"},
				},
			},
		})
	}))
	c := NewClient(sock)
	ip, err := c.InspectContainerBridgeIP(context.Background(), "abc")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if ip != "10.100.0.42" {
		t.Errorf("ip = %q", ip)
	}
}

func TestInspectContainerBridgeIP_AcceptsBridgeInterfaceKey(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ContainerInspect{
			ID: "abc",
			NetworkSettings: ContainerNetworkSettings{
				Networks: map[string]ContainerNetwork{
					PluginBridgeIface: {IPAddress: "10.100.0.43"},
				},
			},
		})
	}))
	c := NewClient(sock)
	ip, err := c.InspectContainerBridgeIP(context.Background(), "abc")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if ip != "10.100.0.43" {
		t.Errorf("ip = %q", ip)
	}
}

func TestInspectContainerBridgeIP_NotAttachedErrors(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ContainerInspect{
			ID: "abc",
			NetworkSettings: ContainerNetworkSettings{
				Networks: map[string]ContainerNetwork{
					"bridge": {IPAddress: "172.17.0.5"},
				},
			},
		})
	}))
	c := NewClient(sock)
	_, err := c.InspectContainerBridgeIP(context.Background(), "abc")
	if err == nil {
		t.Fatal("expected error for unattached container")
	}
	if !strings.Contains(err.Error(), "not attached") {
		t.Errorf("err = %v", err)
	}
}
