package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/network"
)

// NetworkHandler handles /api/network/* endpoints.
type NetworkHandler struct {
	safeApply  *network.SafeApply
	networkDir string
}

func NewNetworkHandler() *NetworkHandler {
	return &NetworkHandler{
		safeApply:  network.NewSafeApply(),
		networkDir: "/etc/systemd/network",
	}
}

// Route dispatches network requests.
func (h *NetworkHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/api/network/interfaces" || path == "/api/network/interfaces/":
		if r.Method == http.MethodGet {
			h.listInterfaces(w, r)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case strings.HasPrefix(path, "/api/network/interfaces/"):
		h.routeInterface(w, r)
	case path == "/api/network/bonds" || path == "/api/network/bonds/":
		h.routeBondsList(w, r)
	case strings.HasPrefix(path, "/api/network/bonds/"):
		h.routeBond(w, r)
	case path == "/api/network/vlans" || path == "/api/network/vlans/":
		h.routeVLANsList(w, r)
	case strings.HasPrefix(path, "/api/network/vlans/"):
		h.routeVLAN(w, r)
	case path == "/api/network/dns":
		h.routeDNS(w, r)
	case path == "/api/network/hostname":
		h.routeHostname(w, r)
	case path == "/api/network/routes" || path == "/api/network/routes/":
		h.routeRoutes(w, r)
	case strings.HasPrefix(path, "/api/network/routes/"):
		h.routeRoute(w, r)
	case path == "/api/network/pending":
		h.getPending(w, r)
	case path == "/api/network/pending/confirm":
		h.confirmPending(w, r)
	case path == "/api/network/pending/revert":
		h.revertPending(w, r)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// --- Interfaces ---

func (h *NetworkHandler) listInterfaces(w http.ResponseWriter, r *http.Request) {
	ifaces, err := network.ListInterfaces()
	if err != nil {
		serverError(w, err)
		return
	}
	if ifaces == nil {
		ifaces = []network.Interface{}
	}
	json.NewEncoder(w).Encode(ifaces)
}

func (h *NetworkHandler) routeInterface(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/network/interfaces/")
	parts := strings.SplitN(rest, "/", 2)
	ifName := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}

	switch subpath {
	case "":
		if r.Method == http.MethodPut {
			h.configureInterface(w, r, ifName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "identify":
		if r.Method == http.MethodPost {
			if err := network.IdentifyInterface(ifName); err != nil {
				jsonError(w, err.Error(), http.StatusInternalServerError)
			} else {
				fmt.Fprintf(w, `{"status":"identifying"}`)
			}
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func (h *NetworkHandler) configureInterface(w http.ResponseWriter, r *http.Request, name string) {
	var cfg network.InterfaceConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	cfg.Name = name

	if err := network.ValidateInterfaceName(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	err := h.safeApply.Apply("Configure interface "+name, func() error {
		return network.WriteConfigFile(h.networkDir, "10-"+name+".network", network.GenerateNetworkFile(cfg))
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
}

// --- Bonds ---

func (h *NetworkHandler) routeBondsList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		bonds, err := network.ListBonds()
		if err != nil {
			serverError(w, err)
			return
		}
		if bonds == nil {
			bonds = []network.BondConfig{}
		}
		json.NewEncoder(w).Encode(bonds)
	case http.MethodPost:
		h.createBond(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (h *NetworkHandler) createBond(w http.ResponseWriter, r *http.Request) {
	var bond network.BondConfig
	if err := json.NewDecoder(r.Body).Decode(&bond); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if err := network.ValidateBondName(bond.Name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := network.ValidateBondMode(bond.Mode); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, member := range bond.Members {
		if err := network.ValidateInterfaceName(member); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	err := h.safeApply.Apply("Create bond "+bond.Name, func() error {
		if err := network.WriteConfigFile(h.networkDir, "05-"+bond.Name+".netdev", network.GenerateBondNetdev(bond)); err != nil {
			return err
		}
		if err := network.WriteConfigFile(h.networkDir, "10-"+bond.Name+".network", network.GenerateBondNetwork(bond)); err != nil {
			return err
		}
		for _, member := range bond.Members {
			if err := network.WriteConfigFile(h.networkDir, "10-"+member+".network", network.GenerateBondMemberNetwork(member, bond.Name)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
}

func (h *NetworkHandler) routeBond(w http.ResponseWriter, r *http.Request) {
	bondName := strings.TrimPrefix(r.URL.Path, "/api/network/bonds/")

	switch r.Method {
	case http.MethodPut:
		h.updateBond(w, r, bondName)
	case http.MethodDelete:
		h.deleteBond(w, r, bondName)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (h *NetworkHandler) updateBond(w http.ResponseWriter, r *http.Request, name string) {
	var bond network.BondConfig
	if err := json.NewDecoder(r.Body).Decode(&bond); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	bond.Name = name

	if err := network.ValidateBondName(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := network.ValidateBondMode(bond.Mode); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, member := range bond.Members {
		if err := network.ValidateInterfaceName(member); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	err := h.safeApply.Apply("Update bond "+name, func() error {
		if err := network.WriteConfigFile(h.networkDir, "05-"+name+".netdev", network.GenerateBondNetdev(bond)); err != nil {
			return err
		}
		if err := network.WriteConfigFile(h.networkDir, "10-"+name+".network", network.GenerateBondNetwork(bond)); err != nil {
			return err
		}
		for _, member := range bond.Members {
			if err := network.WriteConfigFile(h.networkDir, "10-"+member+".network", network.GenerateBondMemberNetwork(member, name)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
}

func (h *NetworkHandler) deleteBond(w http.ResponseWriter, r *http.Request, name string) {
	if err := network.ValidateBondName(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	err := h.safeApply.Apply("Delete bond "+name, func() error {
		// Remove the bond's .netdev and .network files.
		network.RemoveConfigFiles(h.networkDir, "05-"+name+".")
		network.RemoveConfigFiles(h.networkDir, "10-"+name+".")
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
}

// --- VLANs ---

func (h *NetworkHandler) routeVLANsList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		vlans, err := network.ListVLANs()
		if err != nil {
			serverError(w, err)
			return
		}
		if vlans == nil {
			vlans = []network.VLANConfig{}
		}
		json.NewEncoder(w).Encode(vlans)
	case http.MethodPost:
		h.createVLAN(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (h *NetworkHandler) createVLAN(w http.ResponseWriter, r *http.Request) {
	var vlan network.VLANConfig
	if err := json.NewDecoder(r.Body).Decode(&vlan); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if err := network.ValidateVLANID(vlan.ID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := network.ValidateInterfaceName(vlan.Parent); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	vlan.Name = network.VLANName(vlan.Parent, vlan.ID)

	err := h.safeApply.Apply("Create VLAN "+vlan.Name, func() error {
		if err := network.WriteConfigFile(h.networkDir, "05-"+vlan.Name+".netdev", network.GenerateVLANNetdev(vlan)); err != nil {
			return err
		}
		if err := network.WriteConfigFile(h.networkDir, "10-"+vlan.Name+".network", network.GenerateVLANNetwork(vlan)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
}

func (h *NetworkHandler) routeVLAN(w http.ResponseWriter, r *http.Request) {
	vlanName := strings.TrimPrefix(r.URL.Path, "/api/network/vlans/")

	switch r.Method {
	case http.MethodPut:
		h.updateVLAN(w, r, vlanName)
	case http.MethodDelete:
		h.deleteVLAN(w, r, vlanName)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (h *NetworkHandler) updateVLAN(w http.ResponseWriter, r *http.Request, name string) {
	var vlan network.VLANConfig
	if err := json.NewDecoder(r.Body).Decode(&vlan); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	vlan.Name = name

	if err := network.ValidateVLANID(vlan.ID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	err := h.safeApply.Apply("Update VLAN "+name, func() error {
		if err := network.WriteConfigFile(h.networkDir, "05-"+name+".netdev", network.GenerateVLANNetdev(vlan)); err != nil {
			return err
		}
		if err := network.WriteConfigFile(h.networkDir, "10-"+name+".network", network.GenerateVLANNetwork(vlan)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
}

func (h *NetworkHandler) deleteVLAN(w http.ResponseWriter, r *http.Request, name string) {
	err := h.safeApply.Apply("Delete VLAN "+name, func() error {
		network.RemoveConfigFiles(h.networkDir, "05-"+name+".")
		network.RemoveConfigFiles(h.networkDir, "10-"+name+".")
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
}

// --- DNS (no safe-apply) ---

func (h *NetworkHandler) routeDNS(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		dns, err := network.GetDNS()
		if err != nil {
			serverError(w, err)
			return
		}
		json.NewEncoder(w).Encode(dns)
	case http.MethodPut:
		var dns network.DNSConfig
		if err := json.NewDecoder(r.Body).Decode(&dns); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		for _, s := range dns.Servers {
			if err := network.ValidateDNSServer(s); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		for _, d := range dns.SearchDomains {
			if err := network.ValidateSearchDomain(d); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		fmt.Fprintf(w, `{"status":"updated"}`)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// --- Hostname (no safe-apply) ---

func (h *NetworkHandler) routeHostname(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		hostname, err := network.GetHostname()
		if err != nil {
			serverError(w, err)
			return
		}
		fmt.Fprintf(w, `{"hostname":"%s"}`, hostname)
	case http.MethodPut:
		var req struct {
			Hostname string `json:"hostname"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Hostname == "" {
			http.Error(w, `{"error":"hostname required"}`, http.StatusBadRequest)
			return
		}
		if err := network.SetHostname(req.Hostname); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"status":"updated","hostname":"%s"}`, req.Hostname)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// --- Routes ---

func (h *NetworkHandler) routeRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routes, err := network.ListRoutes()
		if err != nil {
			serverError(w, err)
			return
		}
		if routes == nil {
			routes = []network.RouteConfig{}
		}
		json.NewEncoder(w).Encode(routes)
	case http.MethodPost:
		var route network.RouteConfig
		if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if err := network.ValidateRouteCIDR(route.Destination); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if route.Gateway != "" {
			if err := network.ValidateIPv4(route.Gateway); err != nil {
				// Try IPv6.
				if !strings.Contains(route.Gateway, ":") {
					jsonError(w, "invalid gateway: "+route.Gateway, http.StatusBadRequest)
					return
				}
			}
		}
		if route.Interface != "" {
			if err := network.ValidateInterfaceName(route.Interface); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		err := h.safeApply.Apply("Add route to "+route.Destination, func() error {
			return network.WriteConfigFile(h.networkDir, "20-route-"+sanitizeFilename(route.Destination)+".network",
				h.generateRouteNetworkFile(route))
		})
		if err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (h *NetworkHandler) routeRoute(w http.ResponseWriter, r *http.Request) {
	routeID := strings.TrimPrefix(r.URL.Path, "/api/network/routes/")

	if r.Method == http.MethodDelete {
		err := h.safeApply.Apply("Delete route "+routeID, func() error {
			return network.RemoveConfigFiles(h.networkDir, "20-route-"+sanitizeFilename(routeID)+".")
		})
		if err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		fmt.Fprintf(w, `{"status":"applied","confirm_within_seconds":90}`)
	} else {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// generateRouteNetworkFile creates a .network file with a [Route] section.
func (h *NetworkHandler) generateRouteNetworkFile(route network.RouteConfig) string {
	iface := route.Interface
	if iface == "" {
		iface = "*"
	}
	content := "# Auto-generated by tierd. Do not edit.\n"
	content += fmt.Sprintf("[Match]\nName=%s\n\n", iface)
	content += network.GenerateRouteSection([]network.RouteConfig{route})
	return content
}

// sanitizeFilename replaces characters unsafe for filenames.
func sanitizeFilename(s string) string {
	r := strings.NewReplacer("/", "_", ":", "_", " ", "_")
	return r.Replace(s)
}

// --- Safe-apply control ---

func (h *NetworkHandler) getPending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	status := h.safeApply.Status()
	if status == nil {
		fmt.Fprintf(w, "null")
		return
	}
	json.NewEncoder(w).Encode(status)
}

func (h *NetworkHandler) confirmPending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if err := h.safeApply.Confirm(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, `{"status":"confirmed"}`)
}

func (h *NetworkHandler) revertPending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if err := h.safeApply.Revert(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, `{"status":"reverted"}`)
}
