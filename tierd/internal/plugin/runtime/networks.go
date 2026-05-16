package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Network constants for the LXC2Docker-managed plugin bridge. SmoothNAS
// targets the runtime's built-in network rather than creating a second
// Docker network: the create-network endpoint is a Docker API compatibility
// surface, while the actual LXC veth bridge is runtime-managed.
const (
	PluginBridgeName    = "gow"
	PluginBridgeSubnet  = "10.100.0.0/24"
	PluginBridgeGateway = "10.100.0.1"
)

// CreateNetworkRequest is the body of POST /networks/create.
type CreateNetworkRequest struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	IPAM    NetworkIPAM       `json:"IPAM"`
	Options map[string]string `json:"Options,omitempty"`
	Labels  map[string]string `json:"Labels,omitempty"`
}

// NetworkIPAM is the IPAM block of CreateNetworkRequest.
type NetworkIPAM struct {
	Driver string             `json:"Driver,omitempty"` // typically "default"
	Config []NetworkIPAMRange `json:"Config,omitempty"`
}

// NetworkIPAMRange is one entry in NetworkIPAM.Config.
type NetworkIPAMRange struct {
	Subnet  string `json:"Subnet"`
	Gateway string `json:"Gateway,omitempty"`
}

// CreateNetworkResponse is the body returned by POST /networks/create.
type CreateNetworkResponse struct {
	ID      string `json:"Id"`
	Warning string `json:"Warning,omitempty"`
}

// NetworkInspect is the subset of GET /networks/<name> tierd reads.
type NetworkInspect struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Driver string            `json:"Driver"`
	IPAM   NetworkIPAM       `json:"IPAM"`
	Labels map[string]string `json:"Labels"`
}

// CreateNetwork issues POST /networks/create. Returns the network
// ID. Returns IsConflict on a name collision so the caller can
// distinguish "already exists" from real failures.
func (c *Client) CreateNetwork(ctx context.Context, req CreateNetworkRequest) (string, error) {
	var out CreateNetworkResponse
	if err := c.postJSON(ctx, "/networks/create", nil, req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// InspectNetwork issues GET /networks/<name>. Returns IsNotFound
// when the network doesn't exist.
func (c *Client) InspectNetwork(ctx context.Context, name string) (NetworkInspect, error) {
	var out NetworkInspect
	if err := c.getJSON(ctx, "/networks/"+url.PathEscape(name), nil, &out); err != nil {
		return NetworkInspect{}, err
	}
	return out, nil
}

// RemoveNetwork issues DELETE /networks/<name>. Idempotent on 404.
// Phase 04 does not call this in normal operation — the plugin
// bridge persists across plugin uninstalls — but it's needed for
// teardown / test cleanup.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil, nil)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	return nil
}

// EnsurePluginBridge verifies the LXC2Docker-managed bridge network
// exists and returns the network ID. Idempotent — safe to call at
// every tierd startup.
//
// Returns an error if a network of the same name exists with a
// different subnet (operator manually changed it), so the operator
// is forced to reconcile rather than silently inheriting a wrong
// IP range.
func (c *Client) EnsurePluginBridge(ctx context.Context) (string, error) {
	existing, err := c.InspectNetwork(ctx, PluginBridgeName)
	if err == nil {
		// Already there — verify the subnet matches what we expect.
		if !subnetMatches(existing.IPAM, PluginBridgeSubnet) {
			return "", fmt.Errorf("network %s exists with unexpected subnet (want %s); resolve manually",
				PluginBridgeName, PluginBridgeSubnet)
		}
		return existing.ID, nil
	}
	if !IsNotFound(err) {
		return "", fmt.Errorf("inspect %s: %w", PluginBridgeName, err)
	}

	id, err := c.CreateNetwork(ctx, CreateNetworkRequest{
		Name:   PluginBridgeName,
		Driver: "bridge",
		IPAM: NetworkIPAM{
			Driver: "default",
			Config: []NetworkIPAMRange{{
				Subnet:  PluginBridgeSubnet,
				Gateway: PluginBridgeGateway,
			}},
		},
		Labels: map[string]string{
			PluginManagedLabel: "true",
		},
	})
	if err != nil {
		// Race: another caller created it between our inspect and
		// our create. The Docker API returns 409. Treat as success
		// and re-inspect to get the ID.
		if IsConflict(err) {
			again, err2 := c.InspectNetwork(ctx, PluginBridgeName)
			if err2 != nil {
				return "", fmt.Errorf("create raced and re-inspect failed: %w", err2)
			}
			return again.ID, nil
		}
		return "", err
	}
	return id, nil
}

// subnetMatches reports whether any of the IPAM config ranges has
// the given subnet. Naive string match — Docker normalises the
// canonical form so this is safe for the values tierd passes.
func subnetMatches(ipam NetworkIPAM, want string) bool {
	for _, r := range ipam.Config {
		if r.Subnet == want {
			return true
		}
	}
	return false
}

// ErrBridgeIPNotReady is returned by InspectContainerBridgeIP when
// the container is on the bridge but the IPAM hasn't assigned an
// address yet (rare; happens during the brief window between
// container create and start).
var ErrBridgeIPNotReady = errors.New("bridge IP not yet assigned")

// InspectContainerBridgeIP returns the container's IP on the plugin
// bridge. Surfaces ErrBridgeIPNotReady when the bridge is attached
// but the address hasn't been assigned yet, so the caller can retry.
func (c *Client) InspectContainerBridgeIP(ctx context.Context, id string) (string, error) {
	details, err := c.InspectContainer(ctx, id)
	if err != nil {
		return "", err
	}
	net, ok := details.NetworkSettings.Networks[PluginBridgeName]
	if !ok {
		return "", fmt.Errorf("container %s not attached to %s", shortID(id), PluginBridgeName)
	}
	if net.IPAddress == "" {
		return "", ErrBridgeIPNotReady
	}
	return net.IPAddress, nil
}

// shortID truncates a container ID for log + error output.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
