package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	mdadmadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
)

// capabilitiesResponse mirrors tiering.TargetCapabilities for JSON output.
type capabilitiesResponse struct {
	MovementGranularity string `json:"movement_granularity"`
	PinScope            string `json:"pin_scope"`
	SupportsOnlineMove  bool   `json:"supports_online_move"`
	SupportsRecall      bool   `json:"supports_recall"`
	RecallMode          string `json:"recall_mode"`
	SnapshotMode        string `json:"snapshot_mode"`
	SupportsChecksums   bool   `json:"supports_checksums"`
	SupportsCompression bool   `json:"supports_compression"`
	SupportsWriteBias   bool   `json:"supports_write_bias"`
}

// TieringHandler handles all /api/tiering/* requests for the unified
// control-plane API defined in proposal unified-tiering-01-common-model.
//
// The handler reads from and writes to the control-plane SQLite tables.
// Backend adapters (registered via RegisterAdapter) are called for
// reconciliation, movement, and pin operations; they write back to the same
// tables through the control plane.
type TieringHandler struct {
	store         *db.Store
	mu            sync.RWMutex
	adapters      []tiering.TieringAdapter
	lastReconcile time.Time
}

// NewTieringHandler returns a handler bound to the given store.
func NewTieringHandler(store *db.Store) *TieringHandler {
	return &TieringHandler{store: store}
}

// RegisterAdapter adds a backend adapter to the registry. Returns an error if
// an adapter of the same kind is already registered.
func (h *TieringHandler) RegisterAdapter(a tiering.TieringAdapter) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range h.adapters {
		if existing.Kind() == a.Kind() {
			return fmt.Errorf("adapter %q is already registered", a.Kind())
		}
	}
	h.adapters = append(h.adapters, a)
	return nil
}

// UnregisterAdapter removes the adapter with the given kind from the registry
// and marks all running movement jobs for that backend as failed.
func (h *TieringHandler) UnregisterAdapter(kind string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	found := false
	remaining := h.adapters[:0]
	for _, a := range h.adapters {
		if a.Kind() == kind {
			found = true
		} else {
			remaining = append(remaining, a)
		}
	}
	if !found {
		return fmt.Errorf("adapter %q is not registered", kind)
	}
	h.adapters = remaining
	return h.store.MarkRunningJobsFailed(kind, "adapter_unregistered")
}

// Adapters returns a snapshot of the currently registered adapters.
func (h *TieringHandler) Adapters() []tiering.TieringAdapter {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]tiering.TieringAdapter, len(h.adapters))
	copy(out, h.adapters)
	return out
}

// ServeHTTP implements http.Handler by delegating to Route.
func (h *TieringHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Route(w, r)
}

// Route dispatches /api/tiering/* requests.
func (h *TieringHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	// domains
	case path == "/api/tiering/domains":
		if r.Method == http.MethodGet {
			h.listDomains(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}

	case strings.HasPrefix(path, "/api/tiering/domains/"):
		id := strings.TrimPrefix(path, "/api/tiering/domains/")
		if id == "" {
			jsonNotFound(w)
			return
		}
		if r.Method == http.MethodGet {
			h.getDomain(w, r, id)
		} else {
			jsonMethodNotAllowed(w)
		}

	// targets
	case path == "/api/tiering/targets":
		if r.Method == http.MethodGet {
			h.listTargets(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}

	case strings.HasPrefix(path, "/api/tiering/targets/"):
		rest := strings.TrimPrefix(path, "/api/tiering/targets/")
		if rest == "" {
			jsonNotFound(w)
			return
		}
		// /api/tiering/targets/{id}/policy
		if strings.HasSuffix(rest, "/policy") {
			id := strings.TrimSuffix(rest, "/policy")
			if r.Method == http.MethodPut {
				h.updateTargetPolicy(w, r, id)
			} else {
				jsonMethodNotAllowed(w)
			}
			return
		}
		// /api/tiering/targets/{id}
		if r.Method == http.MethodGet {
			h.getTarget(w, r, rest)
		} else {
			jsonMethodNotAllowed(w)
		}

	// namespaces
	case path == "/api/tiering/namespaces":
		switch r.Method {
		case http.MethodGet:
			h.listNamespaces(w, r)
		case http.MethodPost:
			h.createNamespace(w, r)
		default:
			jsonMethodNotAllowed(w)
		}

	case strings.HasPrefix(path, "/api/tiering/namespaces/"):
		h.routeNamespace(w, r, strings.TrimPrefix(path, "/api/tiering/namespaces/"))

	// movements
	case path == "/api/tiering/movements":
		switch r.Method {
		case http.MethodGet:
			h.listMovements(w, r)
		case http.MethodPost:
			h.createMovement(w, r)
		default:
			jsonMethodNotAllowed(w)
		}

	case strings.HasPrefix(path, "/api/tiering/movements/"):
		id := strings.TrimPrefix(path, "/api/tiering/movements/")
		if id == "" {
			jsonNotFound(w)
			return
		}
		if r.Method == http.MethodDelete {
			h.cancelMovement(w, r, id)
		} else {
			jsonMethodNotAllowed(w)
		}

	// meta store stats (per-pool, per-shard)
	case path == "/api/tiering/meta/stats":
		if r.Method == http.MethodGet {
			h.metaStats(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}

	// degraded states
	case path == "/api/tiering/degraded":
		if r.Method == http.MethodGet {
			h.listDegradedStates(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}

	// reconcile
	case path == "/api/tiering/reconcile":
		if r.Method == http.MethodPost {
			h.reconcile(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}

	default:
		jsonNotFound(w)
	}
}

// routeNamespace dispatches sub-paths under /api/tiering/namespaces/{id}/.
func (h *TieringHandler) routeNamespace(w http.ResponseWriter, r *http.Request, rest string) {
	// /api/tiering/namespaces/{id}
	// /api/tiering/namespaces/{id}/pin
	// /api/tiering/namespaces/{id}/snapshot
	// /api/tiering/namespaces/{id}/objects
	// /api/tiering/namespaces/{id}/objects/{object_id}
	// /api/tiering/namespaces/{id}/objects/{object_id}/pin

	if rest == "" {
		jsonNotFound(w)
		return
	}

	parts := strings.SplitN(rest, "/", 3)
	nsID := parts[0]

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.getNamespace(w, r, nsID)
		case http.MethodDelete:
			h.deleteNamespace(w, r, nsID)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	sub := parts[1]
	switch sub {
	case "pin":
		switch r.Method {
		case http.MethodPut:
			h.pinNamespace(w, r, nsID)
		case http.MethodDelete:
			h.unpinNamespace(w, r, nsID)
		default:
			jsonMethodNotAllowed(w)
		}

	case "snapshot":
		// POST /api/tiering/namespaces/{id}/snapshot — create a coordinated snapshot.
		if r.Method == http.MethodPost {
			h.createNamespaceSnapshot(w, r, nsID)
		} else {
			jsonMethodNotAllowed(w)
		}

	case "snapshots":
		if len(parts) == 2 {
			// GET /api/tiering/namespaces/{id}/snapshots — list snapshots.
			if r.Method == http.MethodGet {
				h.listNamespaceSnapshots(w, r, nsID)
			} else {
				jsonMethodNotAllowed(w)
			}
			return
		}
		// /api/tiering/namespaces/{id}/snapshots/{snap_id}
		snapID := parts[2]
		switch r.Method {
		case http.MethodGet:
			h.getNamespaceSnapshot(w, r, nsID, snapID)
		case http.MethodDelete:
			h.deleteNamespaceSnapshot(w, r, nsID, snapID)
		default:
			jsonMethodNotAllowed(w)
		}

	case "files":
		// /api/tiering/namespaces/{id}/files?prefix=foo&limit=500
		if r.Method == http.MethodGet {
			h.listNamespaceFiles(w, r, nsID)
		} else {
			jsonMethodNotAllowed(w)
		}

	case "objects":
		if len(parts) == 2 {
			// /api/tiering/namespaces/{id}/objects
			if r.Method == http.MethodGet {
				h.listObjects(w, r, nsID)
			} else {
				jsonMethodNotAllowed(w)
			}
			return
		}
		// /api/tiering/namespaces/{id}/objects/{object_id}[/pin]
		objectRest := parts[2]
		objectParts := strings.SplitN(objectRest, "/", 2)
		objectID := objectParts[0]
		if len(objectParts) == 1 {
			if r.Method == http.MethodGet {
				h.getObject(w, r, nsID, objectID)
			} else {
				jsonMethodNotAllowed(w)
			}
			return
		}
		if objectParts[1] == "pin" {
			switch r.Method {
			case http.MethodPut:
				h.pinObject(w, r, nsID, objectID)
			case http.MethodDelete:
				h.unpinObject(w, r, nsID, objectID)
			default:
				jsonMethodNotAllowed(w)
			}
			return
		}
		jsonNotFound(w)

	default:
		jsonNotFound(w)
	}
}

// ---- response types ---------------------------------------------------------

type placementDomainResponse struct {
	ID          string `json:"id"`
	BackendKind string `json:"backend_kind"`
	Description string `json:"description"`
	TargetCount int    `json:"target_count"`
	Health      string `json:"health"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type tierTargetResponse struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	PlacementDomain  string               `json:"placement_domain"`
	BackendKind      string               `json:"backend_kind"`
	Rank             int                  `json:"rank"`
	TargetFillPct    int                  `json:"target_fill_pct"`
	FullThresholdPct int                  `json:"full_threshold_pct"`
	PolicyRevision   int64                `json:"policy_revision"`
	Health           string               `json:"health"`
	ActivityBand     string               `json:"activity_band"`
	ActivityTrend    string               `json:"activity_trend"`
	BackingRef       string               `json:"backing_ref"`
	Capabilities     capabilitiesResponse `json:"capabilities"`
	CreatedAt        string               `json:"created_at"`
	UpdatedAt        string               `json:"updated_at"`
}

type managedNamespaceResponse struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	PlacementDomain string          `json:"placement_domain"`
	BackendKind     string          `json:"backend_kind"`
	NamespaceKind   string          `json:"namespace_kind"`
	ExposedPath     string          `json:"exposed_path"`
	PinState        string          `json:"pin_state"`
	IntentRevision  int64           `json:"intent_revision"`
	Health          string          `json:"health"`
	PlacementState  string          `json:"placement_state"`
	BackendRef      string          `json:"backend_ref"`
	CapacityBytes   uint64          `json:"capacity_bytes"`
	UsedBytes       uint64          `json:"used_bytes"`
	PolicyTargetIDs json.RawMessage `json:"policy_target_ids"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
	SnapshotMode    string          `json:"snapshot_mode,omitempty"`
}

type namespaceSnapshotSummaryResponse struct {
	SnapshotID  string `json:"snapshot_id"`
	NamespaceID string `json:"namespace_id"`
	PoolName    string `json:"pool_name"`
	CreatedAt   string `json:"created_at"`
	Consistency string `json:"consistency"`
}

type namespaceSnapshotResponse struct {
	SnapshotID      string                    `json:"snapshot_id"`
	NamespaceID     string                    `json:"namespace_id"`
	PoolName        string                    `json:"pool_name"`
	ZFSSnapshotName string                    `json:"zfs_snapshot_name"`
	BackingSnaps    []tiering.BackingSnapshot `json:"backing_snapshots"`
	MetaSnapshot    tiering.BackingSnapshot   `json:"meta_snapshot"`
	CreatedAt       string                    `json:"created_at"`
	Consistency     string                    `json:"consistency"`
}

type managedObjectResponse struct {
	ID               string          `json:"id"`
	NamespaceID      string          `json:"namespace_id"`
	ObjectKind       string          `json:"object_kind"`
	ObjectKey        string          `json:"object_key"`
	PinState         string          `json:"pin_state"`
	ActivityBand     string          `json:"activity_band"`
	PlacementSummary json.RawMessage `json:"placement_summary"`
	BackendRef       string          `json:"backend_ref"`
	UpdatedAt        string          `json:"updated_at"`
}

type movementJobResponse struct {
	ID              string `json:"id"`
	BackendKind     string `json:"backend_kind"`
	NamespaceID     string `json:"namespace_id"`
	ObjectID        string `json:"object_id,omitempty"`
	MovementUnit    string `json:"movement_unit"`
	PlacementDomain string `json:"placement_domain"`
	SourceTargetID  string `json:"source_target_id"`
	DestTargetID    string `json:"dest_target_id"`
	PolicyRevision  int64  `json:"policy_revision"`
	IntentRevision  int64  `json:"intent_revision"`
	State           string `json:"state"`
	TriggeredBy     string `json:"triggered_by"`
	ProgressBytes   int64  `json:"progress_bytes"`
	TotalBytes      int64  `json:"total_bytes"`
	FailureReason   string `json:"failure_reason,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	UpdatedAt       string `json:"updated_at"`
	CompletedAt     string `json:"completed_at,omitempty"`
}

type degradedStateResponse struct {
	ID          string `json:"id"`
	BackendKind string `json:"backend_kind"`
	ScopeKind   string `json:"scope_kind"`
	ScopeID     string `json:"scope_id"`
	Severity    string `json:"severity"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	UpdatedAt   string `json:"updated_at"`
}

type updateTargetPolicyRequest struct {
	TargetFillPct    int `json:"target_fill_pct"`
	FullThresholdPct int `json:"full_threshold_pct"`
}

type createNamespaceRequest struct {
	Name            string `json:"name"`
	PlacementDomain string `json:"placement_domain"`
	BackendKind     string `json:"backend_kind"`
	NamespaceKind   string `json:"namespace_kind"`
	ExposedPath     string `json:"exposed_path"`
}

type pinRequest struct {
	PinState string `json:"pin_state"`
}

type createMovementRequest struct {
	NamespaceID    string `json:"namespace_id"`
	ObjectID       string `json:"object_id,omitempty"`
	SourceTargetID string `json:"source_target_id"`
	DestTargetID   string `json:"dest_target_id"`
	MovementUnit   string `json:"movement_unit"`
	TriggeredBy    string `json:"triggered_by"`
}

// ---- handlers ---------------------------------------------------------------

func (h *TieringHandler) listDomains(w http.ResponseWriter, r *http.Request) {
	domains, err := h.store.ListPlacementDomains()
	if err != nil {
		serverError(w, err)
		return
	}
	// Count targets per domain.
	targets, err := h.store.ListTierTargets()
	if err != nil {
		serverError(w, err)
		return
	}
	countByDomain := make(map[string]int)
	for _, t := range targets {
		countByDomain[t.PlacementDomain]++
	}

	resp := make([]placementDomainResponse, 0, len(domains))
	for _, d := range domains {
		resp = append(resp, placementDomainResponse{
			ID:          d.ID,
			BackendKind: d.BackendKind,
			Description: d.Description,
			TargetCount: countByDomain[d.ID],
			Health:      "unknown",
			CreatedAt:   d.CreatedAt,
			UpdatedAt:   d.UpdatedAt,
		})
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *TieringHandler) getDomain(w http.ResponseWriter, r *http.Request, id string) {
	d, err := h.store.GetPlacementDomain(id)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "domain not found", http.StatusNotFound, "tiering.domain_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	targets, err := h.store.ListTierTargets()
	if err != nil {
		serverError(w, err)
		return
	}
	var domainTargets []tierTargetResponse
	for _, t := range targets {
		if t.PlacementDomain == id {
			domainTargets = append(domainTargets, dbTargetToResponse(t))
		}
	}
	json.NewEncoder(w).Encode(struct {
		placementDomainResponse
		Targets []tierTargetResponse `json:"targets"`
	}{
		placementDomainResponse: placementDomainResponse{
			ID:          d.ID,
			BackendKind: d.BackendKind,
			Description: d.Description,
			TargetCount: len(domainTargets),
			Health:      "unknown",
			CreatedAt:   d.CreatedAt,
			UpdatedAt:   d.UpdatedAt,
		},
		Targets: domainTargets,
	})
}

func (h *TieringHandler) listTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := h.store.ListTierTargets()
	if err != nil {
		serverError(w, err)
		return
	}
	resp := make([]tierTargetResponse, 0, len(targets))
	for _, t := range targets {
		resp = append(resp, dbTargetToResponse(t))
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *TieringHandler) getTarget(w http.ResponseWriter, r *http.Request, id string) {
	t, err := h.store.GetTierTarget(id)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "target not found", http.StatusNotFound, "tiering.target_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(dbTargetToResponse(*t))
}

func (h *TieringHandler) updateTargetPolicy(w http.ResponseWriter, r *http.Request, id string) {
	var req updateTargetPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	err := h.store.UpdateTierTargetPolicy(id, req.TargetFillPct, req.FullThresholdPct)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "target not found", http.StatusNotFound, "tiering.target_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	t, err := h.store.GetTierTarget(id)
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(dbTargetToResponse(*t))
}

func (h *TieringHandler) listNamespaces(w http.ResponseWriter, r *http.Request) {
	nss, err := h.store.ListManagedNamespaces()
	if err != nil {
		serverError(w, err)
		return
	}
	resp := make([]managedNamespaceResponse, 0, len(nss))
	ca := h.coordinatedSnapshotAdapter()
	for _, ns := range nss {
		item := dbNamespaceToResponse(ns)
		if ca != nil {
			if mode, merr := ca.GetNamespaceSnapshotMode(ns.ID); merr == nil {
				item.SnapshotMode = mode
			}
		}
		resp = append(resp, item)
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *TieringHandler) createNamespace(w http.ResponseWriter, r *http.Request) {
	var req createNamespaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Name == "" {
		jsonErrorCoded(w, "name is required", http.StatusBadRequest, "tiering.name_required")
		return
	}
	if req.PlacementDomain == "" {
		jsonErrorCoded(w, "placement_domain is required", http.StatusBadRequest, "tiering.placement_domain_required")
		return
	}
	kind := req.NamespaceKind
	if kind == "" {
		kind = "volume"
	}
	ns := &db.ManagedNamespaceRow{
		Name:            req.Name,
		PlacementDomain: req.PlacementDomain,
		BackendKind:     req.BackendKind,
		NamespaceKind:   kind,
		ExposedPath:     req.ExposedPath,
		PinState:        "none",
		Health:          "unknown",
		PlacementState:  "unknown",
	}
	if err := h.store.CreateManagedNamespace(ns); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(dbNamespaceToResponse(*ns))
}

// coordinatedSnapshotAdapter returns the first registered adapter that
// implements CoordinatedSnapshotAdapter, or nil if none is registered.
func (h *TieringHandler) coordinatedSnapshotAdapter() tiering.CoordinatedSnapshotAdapter {
	for _, a := range h.adapters {
		if ca, ok := a.(tiering.CoordinatedSnapshotAdapter); ok {
			return ca
		}
	}
	return nil
}

func (h *TieringHandler) getNamespace(w http.ResponseWriter, r *http.Request, id string) {
	ns, err := h.store.GetManagedNamespace(id)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	resp := dbNamespaceToResponse(*ns)
	if ca := h.coordinatedSnapshotAdapter(); ca != nil {
		if mode, merr := ca.GetNamespaceSnapshotMode(id); merr == nil {
			resp.SnapshotMode = mode
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *TieringHandler) deleteNamespace(w http.ResponseWriter, r *http.Request, id string) {
	err := h.store.DeleteManagedNamespace(id)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *TieringHandler) pinNamespace(w http.ResponseWriter, r *http.Request, id string) {
	var req pinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.PinState == "" {
		req.PinState = "pinned-hot"
	}
	err := h.store.SetNamespacePinState(id, req.PinState)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	ns, err := h.store.GetManagedNamespace(id)
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(dbNamespaceToResponse(*ns))
}

func (h *TieringHandler) unpinNamespace(w http.ResponseWriter, r *http.Request, id string) {
	err := h.store.SetNamespacePinState(id, "none")
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	ns, err := h.store.GetManagedNamespace(id)
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(dbNamespaceToResponse(*ns))
}

func (h *TieringHandler) createNamespaceSnapshot(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := h.store.GetManagedNamespace(id); errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	ca := h.coordinatedSnapshotAdapter()
	if ca == nil {
		jsonErrorCoded(w, "no adapter registered that supports coordinated snapshots", http.StatusServiceUnavailable, "tiering.snapshot_adapter_unavailable")
		return
	}
	snap, err := ca.CreateNamespaceSnapshot(id)
	if err != nil {
		var ae *tiering.AdapterError
		if errors.As(err, &ae) {
			switch ae.Kind {
			case tiering.ErrCapabilityViolation:
				// SnapshotMode != coordinated-namespace.
				jsonError(w, ae.Message, http.StatusUnprocessableEntity)
				return
			case tiering.ErrPermanent:
				jsonError(w, ae.Message, http.StatusUnprocessableEntity)
				return
			case tiering.ErrTransient:
				if strings.Contains(ae.Message, "snapshot_in_progress") {
					jsonError(w, ae.Message, http.StatusConflict)
					return
				}
			}
		}
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(snapshotToResponse(snap))
}

func (h *TieringHandler) listNamespaceSnapshots(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := h.store.GetManagedNamespace(id); errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	ca := h.coordinatedSnapshotAdapter()
	if ca == nil {
		json.NewEncoder(w).Encode([]namespaceSnapshotSummaryResponse{})
		return
	}
	summaries, err := ca.ListNamespaceSnapshots(id)
	if err != nil {
		serverError(w, err)
		return
	}
	resp := make([]namespaceSnapshotSummaryResponse, 0, len(summaries))
	for _, s := range summaries {
		resp = append(resp, namespaceSnapshotSummaryResponse{
			SnapshotID:  s.SnapshotID,
			NamespaceID: s.NamespaceID,
			PoolName:    s.PoolName,
			CreatedAt:   s.CreatedAt,
			Consistency: s.Consistency,
		})
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *TieringHandler) getNamespaceSnapshot(w http.ResponseWriter, r *http.Request, id, snapID string) {
	if _, err := h.store.GetManagedNamespace(id); errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	ca := h.coordinatedSnapshotAdapter()
	if ca == nil {
		jsonErrorCoded(w, "snapshot not found", http.StatusNotFound, "tiering.snapshot_not_found")
		return
	}
	snap, err := ca.GetNamespaceSnapshot(id, snapID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "snapshot not found", http.StatusNotFound, "tiering.snapshot_not_found")
			return
		}
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(snapshotToResponse(snap))
}

func (h *TieringHandler) deleteNamespaceSnapshot(w http.ResponseWriter, r *http.Request, id, snapID string) {
	if _, err := h.store.GetManagedNamespace(id); errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	ca := h.coordinatedSnapshotAdapter()
	if ca == nil {
		jsonErrorCoded(w, "snapshot not found", http.StatusNotFound, "tiering.snapshot_not_found")
		return
	}
	if err := ca.DeleteNamespaceSnapshot(id, snapID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "snapshot not found", http.StatusNotFound, "tiering.snapshot_not_found")
			return
		}
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func snapshotToResponse(s *tiering.NamespaceSnapshot) namespaceSnapshotResponse {
	backing := s.BackingSnaps
	if backing == nil {
		backing = []tiering.BackingSnapshot{}
	}
	return namespaceSnapshotResponse{
		SnapshotID:      s.SnapshotID,
		NamespaceID:     s.NamespaceID,
		PoolName:        s.PoolName,
		ZFSSnapshotName: s.ZFSSnapshotName,
		BackingSnaps:    backing,
		MetaSnapshot:    s.MetaSnapshot,
		CreatedAt:       s.CreatedAt,
		Consistency:     s.Consistency,
	}
}

func (h *TieringHandler) listObjects(w http.ResponseWriter, r *http.Request, nsID string) {
	if _, err := h.store.GetManagedNamespace(nsID); errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	objs, err := h.store.ListManagedObjects(nsID)
	if err != nil {
		serverError(w, err)
		return
	}
	resp := make([]managedObjectResponse, 0, len(objs))
	for _, obj := range objs {
		resp = append(resp, dbObjectToResponse(obj))
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *TieringHandler) getObject(w http.ResponseWriter, r *http.Request, nsID, objectID string) {
	obj, err := h.store.GetManagedObject(objectID)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "object not found", http.StatusNotFound, "tiering.object_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if obj.NamespaceID != nsID {
		jsonErrorCoded(w, "object not found", http.StatusNotFound, "tiering.object_not_found")
		return
	}
	json.NewEncoder(w).Encode(dbObjectToResponse(*obj))
}

// pathPinAdapter is the subset of adapter surface the pin handler needs to
// address objects by their namespace-relative path. Adapters that opt in
// implement these methods; the mdadm adapter routes the write through its
// per-pool meta store on the fastest tier.
type pathPinAdapter interface {
	SetObjectPinStateByPath(namespaceID, key, pinState string) error
	GetObjectPinStateByPath(namespaceID, key string) (string, error)
}

// metaStatsAdapter is implemented by adapters that maintain a per-pool
// metadata store and can report diagnostic counters for it.
type metaStatsAdapter interface {
	MetaStats() map[string][]meta.ShardStats
}

// fileListAdapter is implemented by adapters that can enumerate files in
// a namespace along with their pin state.
type fileListAdapter interface {
	ListNamespaceFiles(namespaceID, prefix string, limit int) ([]mdadmadapter.FileEntry, error)
}

func (h *TieringHandler) listNamespaceFiles(w http.ResponseWriter, r *http.Request, nsID string) {
	prefix := r.URL.Query().Get("prefix")
	limit := 500
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	h.mu.RLock()
	adapters := append([]tiering.TieringAdapter(nil), h.adapters...)
	h.mu.RUnlock()
	for _, a := range adapters {
		fa, ok := a.(fileListAdapter)
		if !ok {
			continue
		}
		entries, err := fa.ListNamespaceFiles(nsID, prefix, limit)
		if err != nil {
			// Namespace not on this adapter — try next.
			continue
		}
		if entries == nil {
			entries = []mdadmadapter.FileEntry{}
		}
		_ = json.NewEncoder(w).Encode(entries)
		return
	}
	jsonErrorCoded(w, "namespace not found", http.StatusNotFound, "tiering.namespace_not_found")
}

func (h *TieringHandler) metaStats(w http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	adapters := append([]tiering.TieringAdapter(nil), h.adapters...)
	h.mu.RUnlock()
	out := make(map[string][]meta.ShardStats)
	for _, a := range adapters {
		ms, ok := a.(metaStatsAdapter)
		if !ok {
			continue
		}
		for pool, shards := range ms.MetaStats() {
			out[pool] = shards
		}
	}
	json.NewEncoder(w).Encode(out)
}

// setPinByPath walks adapters and dispatches to the first one that accepts
// the namespace. Returns (handled, error) — handled=false means no adapter
// knew about this namespace, which the caller should treat as 404.
func (h *TieringHandler) setPinByPath(nsID, key, state string) (bool, error) {
	h.mu.RLock()
	adapters := append([]tiering.TieringAdapter(nil), h.adapters...)
	h.mu.RUnlock()
	for _, a := range adapters {
		pa, ok := a.(pathPinAdapter)
		if !ok {
			continue
		}
		if err := pa.SetObjectPinStateByPath(nsID, key, state); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// looksLikePath returns true when objectID should be treated as a
// namespace-relative path rather than a managed_objects row ID. Legacy IDs
// are sqlid / UUID-shaped (no slashes, no dots); paths contain slashes.
func looksLikePath(objectID string) bool {
	return strings.ContainsAny(objectID, "/.")
}

func (h *TieringHandler) pinObject(w http.ResponseWriter, r *http.Request, nsID, objectID string) {
	var req pinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.PinState == "" {
		req.PinState = "pinned-hot"
	}

	// Path-based fast path for files tracked in the meta store.
	if looksLikePath(objectID) {
		handled, err := h.setPinByPath(nsID, objectID, req.PinState)
		if err != nil {
			serverError(w, err)
			return
		}
		if handled {
			// Echo back a minimal response; caller typically ignores the body.
			_ = json.NewEncoder(w).Encode(map[string]string{
				"pin_state": req.PinState,
				"object":    objectID,
			})
			return
		}
		jsonErrorCoded(w, "object not found", http.StatusNotFound, "tiering.object_not_found")
		return
	}

	// Legacy UUID-keyed row in managed_objects.
	obj, err := h.store.GetManagedObject(objectID)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "object not found", http.StatusNotFound, "tiering.object_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if obj.NamespaceID != nsID {
		jsonErrorCoded(w, "object not found", http.StatusNotFound, "tiering.object_not_found")
		return
	}
	if err := h.store.SetObjectPinState(objectID, req.PinState); err != nil {
		serverError(w, err)
		return
	}
	updated, err := h.store.GetManagedObject(objectID)
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(dbObjectToResponse(*updated))
}

func (h *TieringHandler) unpinObject(w http.ResponseWriter, r *http.Request, nsID, objectID string) {
	if looksLikePath(objectID) {
		handled, err := h.setPinByPath(nsID, objectID, "none")
		if err != nil {
			serverError(w, err)
			return
		}
		if handled {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"pin_state": "none",
				"object":    objectID,
			})
			return
		}
		jsonErrorCoded(w, "object not found", http.StatusNotFound, "tiering.object_not_found")
		return
	}

	obj, err := h.store.GetManagedObject(objectID)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "object not found", http.StatusNotFound, "tiering.object_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if obj.NamespaceID != nsID {
		jsonErrorCoded(w, "object not found", http.StatusNotFound, "tiering.object_not_found")
		return
	}
	if err := h.store.SetObjectPinState(objectID, "none"); err != nil {
		serverError(w, err)
		return
	}
	updated, err := h.store.GetManagedObject(objectID)
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(dbObjectToResponse(*updated))
}

func (h *TieringHandler) listMovements(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.store.ListMovementJobs()
	if err != nil {
		serverError(w, err)
		return
	}
	resp := make([]movementJobResponse, 0, len(jobs))
	for _, j := range jobs {
		resp = append(resp, dbJobToResponse(j))
	}
	json.NewEncoder(w).Encode(resp)
}

// createMovement creates a manual movement job. Returns HTTP 422 if the source
// and destination targets are in different placement domains.
func (h *TieringHandler) createMovement(w http.ResponseWriter, r *http.Request) {
	var req createMovementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.NamespaceID == "" || req.SourceTargetID == "" || req.DestTargetID == "" {
		jsonErrorCoded(w, "namespace_id, source_target_id, and dest_target_id are required", http.StatusBadRequest, "tiering.move_params_required")
		return
	}

	job := &db.MovementJobRow{
		NamespaceID:    req.NamespaceID,
		ObjectID:       req.ObjectID,
		SourceTargetID: req.SourceTargetID,
		DestTargetID:   req.DestTargetID,
		MovementUnit:   req.MovementUnit,
		TriggeredBy:    req.TriggeredBy,
		State:          db.MovementJobStatePending,
	}
	if err := h.store.CreateMovementJob(job); errors.Is(err, db.ErrCrossDomainMovement) {
		jsonErrorCoded(w, "source and destination targets must be in the same placement domain", http.StatusUnprocessableEntity, "tiering.move_cross_domain")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(dbJobToResponse(*job))
}

func (h *TieringHandler) cancelMovement(w http.ResponseWriter, r *http.Request, id string) {
	err := h.store.CancelMovementJob(id)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "movement not found", http.StatusNotFound, "tiering.movement_not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *TieringHandler) listDegradedStates(w http.ResponseWriter, r *http.Request) {
	states, err := h.store.ListDegradedStates()
	if err != nil {
		serverError(w, err)
		return
	}
	resp := make([]degradedStateResponse, 0, len(states))
	for _, d := range states {
		resp = append(resp, degradedStateResponse{
			ID:          d.ID,
			BackendKind: d.BackendKind,
			ScopeKind:   d.ScopeKind,
			ScopeID:     d.ScopeID,
			Severity:    d.Severity,
			Code:        d.Code,
			Message:     d.Message,
			UpdatedAt:   d.UpdatedAt,
		})
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *TieringHandler) reconcile(w http.ResponseWriter, r *http.Request) {
	blocked, err := spindownBlockedTierPools(h.store, spindownNow())
	if err != nil {
		serverError(w, err)
		return
	}
	if len(blocked) > 0 {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":         "reconcile is outside one or more spindown active windows",
			"blocked_pools": blocked,
			"maintenance":   "deferred",
		})
		return
	}

	// Debounce: reject calls within reconcile_debounce_seconds of the last reconcile.
	debounceSeconds := 60
	if val, err := h.store.GetControlPlaneConfig("reconcile_debounce_seconds"); err == nil && val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			debounceSeconds = n
		}
	}
	h.mu.Lock()
	since := time.Since(h.lastReconcile)
	debounce := time.Duration(debounceSeconds) * time.Second
	if !h.lastReconcile.IsZero() && since < debounce {
		remaining := int(math.Ceil((debounce - since).Seconds()))
		h.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(remaining))
		jsonErrorCoded(w, "reconcile called too frequently; try again later", http.StatusTooManyRequests, "tiering.reconcile_rate_limited")
		return
	}
	h.lastReconcile = time.Now()
	h.mu.Unlock()

	adapters := h.Adapters()
	var errs []string
	for _, a := range adapters {
		if err := a.Reconcile(); err != nil {
			errs = append(errs, a.Kind()+": "+err.Error())
		}
	}
	if err := h.store.RecordReconcileTimestamp(); err != nil {
		errs = append(errs, "record timestamp: "+err.Error())
	}
	if len(errs) > 0 {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "partial",
			"errors": errs,
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ---- conversion helpers -----------------------------------------------------

func dbTargetToResponse(t db.TierTargetRow) tierTargetResponse {
	var caps tiering.TargetCapabilities
	if t.CapabilitiesJSON != "" && t.CapabilitiesJSON != "{}" {
		_ = json.Unmarshal([]byte(t.CapabilitiesJSON), &caps)
	}
	return tierTargetResponse{
		ID:               t.ID,
		Name:             t.Name,
		PlacementDomain:  t.PlacementDomain,
		BackendKind:      t.BackendKind,
		Rank:             t.Rank,
		TargetFillPct:    t.TargetFillPct,
		FullThresholdPct: t.FullThresholdPct,
		PolicyRevision:   t.PolicyRevision,
		Health:           t.Health,
		ActivityBand:     t.ActivityBand,
		ActivityTrend:    t.ActivityTrend,
		BackingRef:       t.BackingRef,
		Capabilities: capabilitiesResponse{
			MovementGranularity: caps.MovementGranularity,
			PinScope:            caps.PinScope,
			SupportsOnlineMove:  caps.SupportsOnlineMove,
			SupportsRecall:      caps.SupportsRecall,
			RecallMode:          caps.RecallMode,
			SnapshotMode:        caps.SnapshotMode,
			SupportsChecksums:   caps.SupportsChecksums,
			SupportsCompression: caps.SupportsCompression,
			SupportsWriteBias:   caps.SupportsWriteBias,
		},
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

func dbNamespaceToResponse(ns db.ManagedNamespaceRow) managedNamespaceResponse {
	policyIDs := json.RawMessage(ns.PolicyTargetIDsJSON)
	if len(policyIDs) == 0 {
		policyIDs = json.RawMessage("[]")
	}
	return managedNamespaceResponse{
		ID:              ns.ID,
		Name:            ns.Name,
		PlacementDomain: ns.PlacementDomain,
		BackendKind:     ns.BackendKind,
		NamespaceKind:   ns.NamespaceKind,
		ExposedPath:     ns.ExposedPath,
		PinState:        ns.PinState,
		IntentRevision:  ns.IntentRevision,
		Health:          ns.Health,
		PlacementState:  ns.PlacementState,
		BackendRef:      ns.BackendRef,
		CapacityBytes:   ns.CapacityBytes,
		UsedBytes:       ns.UsedBytes,
		PolicyTargetIDs: policyIDs,
		CreatedAt:       ns.CreatedAt,
		UpdatedAt:       ns.UpdatedAt,
	}
}

func dbObjectToResponse(obj db.ManagedObjectRow) managedObjectResponse {
	summary := json.RawMessage(obj.PlacementSummaryJSON)
	if len(summary) == 0 {
		summary = json.RawMessage("{}")
	}
	return managedObjectResponse{
		ID:               obj.ID,
		NamespaceID:      obj.NamespaceID,
		ObjectKind:       obj.ObjectKind,
		ObjectKey:        obj.ObjectKey,
		PinState:         obj.PinState,
		ActivityBand:     obj.ActivityBand,
		PlacementSummary: summary,
		BackendRef:       obj.BackendRef,
		UpdatedAt:        obj.UpdatedAt,
	}
}

func dbJobToResponse(j db.MovementJobRow) movementJobResponse {
	return movementJobResponse{
		ID:              j.ID,
		BackendKind:     j.BackendKind,
		NamespaceID:     j.NamespaceID,
		ObjectID:        j.ObjectID,
		MovementUnit:    j.MovementUnit,
		PlacementDomain: j.PlacementDomain,
		SourceTargetID:  j.SourceTargetID,
		DestTargetID:    j.DestTargetID,
		PolicyRevision:  j.PolicyRevision,
		IntentRevision:  j.IntentRevision,
		State:           j.State,
		TriggeredBy:     j.TriggeredBy,
		ProgressBytes:   j.ProgressBytes,
		TotalBytes:      j.TotalBytes,
		FailureReason:   j.FailureReason,
		StartedAt:       j.StartedAt,
		UpdatedAt:       j.UpdatedAt,
		CompletedAt:     j.CompletedAt,
	}
}
