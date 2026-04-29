package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
)

// newTieringTestHandler returns a TieringHandler backed by a fresh in-memory
// SQLite store.
func newTieringTestHandler(t *testing.T) *TieringHandler {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "tiering_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return NewTieringHandler(store)
}

// doRequest is a helper that fires an HTTP request against the handler and
// returns the recorder.
func doRequest(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---- empty-DB list endpoints must return [] not 404/500 --------------------

func TestTieringListEndpointsEmptyDB(t *testing.T) {
	h := newTieringTestHandler(t)

	endpoints := []string{
		"/api/tiering/domains",
		"/api/tiering/targets",
		"/api/tiering/namespaces",
		"/api/tiering/movements",
		"/api/tiering/degraded",
	}

	for _, ep := range endpoints {
		rec := doRequest(t, h, http.MethodGet, ep, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", ep, rec.Code)
		}
		var list json.RawMessage
		if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
			t.Errorf("GET %s: body is not valid JSON: %v", ep, err)
			continue
		}
		// Must be an array (possibly empty).
		if !strings.HasPrefix(strings.TrimSpace(string(list)), "[") {
			t.Errorf("GET %s: body = %q, want JSON array", ep, list)
		}
	}
}

// ---- reconcile with no adapters returns ok ---------------------------------

func TestTieringReconcileNoAdapters(t *testing.T) {
	h := newTieringTestHandler(t)
	rec := doRequest(t, h, http.MethodPost, "/api/tiering/reconcile", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/tiering/reconcile: status %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode reconcile response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("status = %q, want ok", resp["status"])
	}
}

// ---- reconcile with a stub adapter -----------------------------------------

// stubAdapter satisfies the TieringAdapter interface and records calls.
type stubAdapter struct {
	kind          string
	reconcileCalls int
}

func (s *stubAdapter) Kind() string           { return s.kind }
func (s *stubAdapter) Reconcile() error       { s.reconcileCalls++; return nil }
func (s *stubAdapter) CollectActivity() ([]tiering.ActivitySample, error) {
	return nil, nil
}
func (s *stubAdapter) CreateTarget(tiering.TargetSpec) (*tiering.TargetState, error) {
	return &tiering.TargetState{}, nil
}
func (s *stubAdapter) DestroyTarget(string) error { return nil }
func (s *stubAdapter) ListTargets() ([]tiering.TargetState, error) { return nil, nil }
func (s *stubAdapter) CreateNamespace(tiering.NamespaceSpec) (*tiering.NamespaceState, error) {
	return &tiering.NamespaceState{}, nil
}
func (s *stubAdapter) DestroyNamespace(string) error { return nil }
func (s *stubAdapter) ListNamespaces() ([]tiering.NamespaceState, error) { return nil, nil }
func (s *stubAdapter) ListManagedObjects(string) ([]tiering.ManagedObjectState, error) {
	return nil, nil
}
func (s *stubAdapter) GetCapabilities(string) (tiering.TargetCapabilities, error) {
	return tiering.TargetCapabilities{}, nil
}
func (s *stubAdapter) GetPolicy(string) (tiering.TargetPolicy, error) {
	return tiering.TargetPolicy{}, nil
}
func (s *stubAdapter) SetPolicy(string, tiering.TargetPolicy) error { return nil }
func (s *stubAdapter) PlanMovements() ([]tiering.MovementPlan, error) { return nil, nil }
func (s *stubAdapter) StartMovement(tiering.MovementPlan) (string, error) { return "", nil }
func (s *stubAdapter) GetMovement(string) (*tiering.MovementState, error) {
	return &tiering.MovementState{}, nil
}
func (s *stubAdapter) CancelMovement(string) error { return nil }
func (s *stubAdapter) Pin(tiering.PinScope, string, string) error { return nil }
func (s *stubAdapter) Unpin(tiering.PinScope, string, string) error { return nil }
func (s *stubAdapter) GetDegradedState() ([]tiering.DegradedState, error) { return nil, nil }

func TestTieringReconcileCallsAdapters(t *testing.T) {
	h := newTieringTestHandler(t)
	stub := &stubAdapter{kind: "stub"}
	h.RegisterAdapter(stub)

	rec := doRequest(t, h, http.MethodPost, "/api/tiering/reconcile", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile: status %d, want 200", rec.Code)
	}
	if stub.reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", stub.reconcileCalls)
	}
}

// ---- target CRUD -----------------------------------------------------------

func TestTieringTargetCRUD(t *testing.T) {
	h := newTieringTestHandler(t)

	// Seed a target directly in the store (adapters would do this in practice).
	tgt := &db.TierTargetRow{
		Name:             "nvme0",
		PlacementDomain:  "fast",
		BackendKind:      "stub",
		Rank:             1,
		TargetFillPct:    50,
		FullThresholdPct: 95,
		Health:           "healthy",
	}
	if err := h.store.CreateTierTarget(tgt); err != nil {
		t.Fatalf("CreateTierTarget: %v", err)
	}

	// List targets: must include the new one.
	rec := doRequest(t, h, http.MethodGet, "/api/tiering/targets", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tiering/targets: %d", rec.Code)
	}
	var list []tierTargetResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 target, got %d", len(list))
	}
	if list[0].Name != "nvme0" {
		t.Fatalf("target name = %q, want nvme0", list[0].Name)
	}

	// Get target by id.
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/targets/"+tgt.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET target by id: %d", rec.Code)
	}

	// Update policy.
	rec = doRequest(t, h, http.MethodPut, "/api/tiering/targets/"+tgt.ID+"/policy",
		map[string]int{"target_fill_pct": 70, "full_threshold_pct": 90})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /policy: %d; body=%s", rec.Code, rec.Body)
	}
	var updated tierTargetResponse
	if err := json.NewDecoder(rec.Body).Decode(&updated); err != nil {
		t.Fatalf("decode updated target: %v", err)
	}
	if updated.TargetFillPct != 70 {
		t.Fatalf("target_fill_pct = %d, want 70", updated.TargetFillPct)
	}
	if updated.PolicyRevision != 2 {
		t.Fatalf("policy_revision = %d, want 2", updated.PolicyRevision)
	}

	// Domain was auto-created.
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/domains", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET domains: %d", rec.Code)
	}
	var domains []placementDomainResponse
	if err := json.NewDecoder(rec.Body).Decode(&domains); err != nil {
		t.Fatalf("decode domains: %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("expected 1 domain, got %d", len(domains))
	}
	if domains[0].ID != "fast" {
		t.Fatalf("domain id = %q, want fast", domains[0].ID)
	}
}

// ---- namespace CRUD --------------------------------------------------------

func TestTieringNamespaceCRUD(t *testing.T) {
	h := newTieringTestHandler(t)

	// Create namespace.
	rec := doRequest(t, h, http.MethodPost, "/api/tiering/namespaces", map[string]string{
		"name":             "vol1",
		"placement_domain": "fast",
		"backend_kind":     "stub",
		"namespace_kind":   "volume",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /namespaces: %d; body=%s", rec.Code, rec.Body)
	}
	var ns managedNamespaceResponse
	if err := json.NewDecoder(rec.Body).Decode(&ns); err != nil {
		t.Fatalf("decode namespace: %v", err)
	}
	if ns.Name != "vol1" {
		t.Fatalf("namespace name = %q, want vol1", ns.Name)
	}
	if ns.ID == "" {
		t.Fatal("namespace id must not be empty")
	}

	// List namespaces.
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/namespaces", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /namespaces: %d", rec.Code)
	}
	var list []managedNamespaceResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode namespace list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 namespace, got %d", len(list))
	}

	// Get by id.
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/namespaces/"+ns.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET namespace by id: %d", rec.Code)
	}

	// Pin namespace.
	rec = doRequest(t, h, http.MethodPut, "/api/tiering/namespaces/"+ns.ID+"/pin",
		map[string]string{"pin_state": "pinned-hot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT pin: %d; body=%s", rec.Code, rec.Body)
	}
	var pinned managedNamespaceResponse
	if err := json.NewDecoder(rec.Body).Decode(&pinned); err != nil {
		t.Fatalf("decode pinned: %v", err)
	}
	if pinned.PinState != "pinned-hot" {
		t.Fatalf("pin_state = %q, want pinned-hot", pinned.PinState)
	}

	// Unpin namespace.
	rec = doRequest(t, h, http.MethodDelete, "/api/tiering/namespaces/"+ns.ID+"/pin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE pin: %d; body=%s", rec.Code, rec.Body)
	}

	// Delete namespace.
	rec = doRequest(t, h, http.MethodDelete, "/api/tiering/namespaces/"+ns.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE namespace: %d", rec.Code)
	}

	// List should now be empty.
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/namespaces", nil)
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 namespaces after delete, got %d", len(list))
	}
}

// ---- domain detail with rank-sorted targets --------------------------------

func TestTieringDomainDetail(t *testing.T) {
	h := newTieringTestHandler(t)

	for _, tgt := range []*db.TierTargetRow{
		{Name: "hdd0", PlacementDomain: "archive", BackendKind: "stub", Rank: 3},
		{Name: "ssd0", PlacementDomain: "archive", BackendKind: "stub", Rank: 2},
		{Name: "nvme0", PlacementDomain: "archive", BackendKind: "stub", Rank: 1},
	} {
		if err := h.store.CreateTierTarget(tgt); err != nil {
			t.Fatalf("CreateTierTarget: %v", err)
		}
	}

	rec := doRequest(t, h, http.MethodGet, "/api/tiering/domains/archive", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET domain detail: %d; body=%s", rec.Code, rec.Body)
	}
	var detail struct {
		ID          string               `json:"id"`
		TargetCount int                  `json:"target_count"`
		Targets     []tierTargetResponse `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("decode domain detail: %v", err)
	}
	if detail.ID != "archive" {
		t.Fatalf("domain id = %q, want archive", detail.ID)
	}
	if detail.TargetCount != 3 {
		t.Fatalf("target_count = %d, want 3", detail.TargetCount)
	}
	if len(detail.Targets) != 3 {
		t.Fatalf("targets len = %d, want 3", len(detail.Targets))
	}
}

// ---- 404 for unknown resources ---------------------------------------------

func TestTieringNotFound(t *testing.T) {
	h := newTieringTestHandler(t)

	notFound := []struct{ method, path string }{
		{http.MethodGet, "/api/tiering/domains/does-not-exist"},
		{http.MethodGet, "/api/tiering/targets/does-not-exist"},
		{http.MethodGet, "/api/tiering/namespaces/does-not-exist"},
		{http.MethodDelete, "/api/tiering/movements/does-not-exist"},
	}
	for _, tc := range notFound {
		rec := doRequest(t, h, tc.method, tc.path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// ---- degraded states -------------------------------------------------------

func TestTieringDegradedStatesListAndContent(t *testing.T) {
	h := newTieringTestHandler(t)

	d := &db.DegradedStateRow{
		BackendKind: "stub",
		ScopeKind:   "target",
		ScopeID:     "tgt-1",
		Severity:    "critical",
		Code:        "array_degraded",
		Message:     "RAID array degraded",
	}
	if err := h.store.UpsertDegradedState(d); err != nil {
		t.Fatalf("UpsertDegradedState: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/api/tiering/degraded", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /degraded: %d", rec.Code)
	}
	var list []degradedStateResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode degraded list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 degraded state, got %d", len(list))
	}
	if list[0].Code != "array_degraded" {
		t.Fatalf("code = %q, want array_degraded", list[0].Code)
	}
}

// ---- capabilities in target response ----------------------------------------

func TestTieringTargetCapabilitiesInResponse(t *testing.T) {
	h := newTieringTestHandler(t)

	capsJSON := `{"movement_granularity":"file","pin_scope":"namespace","supports_recall":true,"recall_mode":"asynchronous","snapshot_mode":"none"}`
	tgt := &db.TierTargetRow{
		Name:             "zfs-t1",
		PlacementDomain:  "zfsdom",
		BackendKind:      "zfsmgd",
		Rank:             1,
		Health:           "healthy",
		CapabilitiesJSON: capsJSON,
	}
	if err := h.store.CreateTierTarget(tgt); err != nil {
		t.Fatalf("CreateTierTarget: %v", err)
	}

	// List targets must include parsed capabilities.
	rec := doRequest(t, h, http.MethodGet, "/api/tiering/targets", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tiering/targets: %d", rec.Code)
	}
	var list []tierTargetResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode target list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 target, got %d", len(list))
	}
	caps := list[0].Capabilities
	if caps.MovementGranularity != "file" {
		t.Errorf("movement_granularity = %q, want file", caps.MovementGranularity)
	}
	if !caps.SupportsRecall {
		t.Error("supports_recall must be true")
	}
	if caps.RecallMode != "asynchronous" {
		t.Errorf("recall_mode = %q, want asynchronous", caps.RecallMode)
	}
	if caps.SnapshotMode != "none" {
		t.Errorf("snapshot_mode = %q, want none", caps.SnapshotMode)
	}

	// Get by ID must also include capabilities.
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/targets/"+tgt.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET target by id: %d", rec.Code)
	}
	var single tierTargetResponse
	if err := json.NewDecoder(rec.Body).Decode(&single); err != nil {
		t.Fatalf("decode single target: %v", err)
	}
	if single.Capabilities.MovementGranularity != "file" {
		t.Errorf("single target movement_granularity = %q, want file", single.Capabilities.MovementGranularity)
	}
}

// ---- mixed-backend domain grouping ------------------------------------------

// TestTieringMixedBackendDomainGrouping verifies that mdadm and zfsmgd targets
// in different placement domains appear in the correct domain, and that each
// domain only contains its own targets.
func TestTieringMixedBackendDomainGrouping(t *testing.T) {
	h := newTieringTestHandler(t)

	targets := []*db.TierTargetRow{
		{Name: "mdadm-ssd", PlacementDomain: "fast", BackendKind: "mdadm", Rank: 1},
		{Name: "mdadm-hdd", PlacementDomain: "fast", BackendKind: "mdadm", Rank: 2},
		{Name: "zfs-nvme", PlacementDomain: "archive", BackendKind: "zfsmgd", Rank: 1},
	}
	for _, tgt := range targets {
		if err := h.store.CreateTierTarget(tgt); err != nil {
			t.Fatalf("CreateTierTarget %s: %v", tgt.Name, err)
		}
	}

	// GET /api/tiering/domains must return 2 domains.
	rec := doRequest(t, h, http.MethodGet, "/api/tiering/domains", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /domains: %d", rec.Code)
	}
	var domains []placementDomainResponse
	if err := json.NewDecoder(rec.Body).Decode(&domains); err != nil {
		t.Fatalf("decode domains: %v", err)
	}
	if len(domains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(domains))
	}

	// GET /api/tiering/domains/fast must have 2 targets ranked 1 and 2.
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/domains/fast", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /domains/fast: %d", rec.Code)
	}
	var fastDetail struct {
		ID          string               `json:"id"`
		TargetCount int                  `json:"target_count"`
		Targets     []tierTargetResponse `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&fastDetail); err != nil {
		t.Fatalf("decode fast domain: %v", err)
	}
	if fastDetail.TargetCount != 2 {
		t.Fatalf("fast domain target_count = %d, want 2", fastDetail.TargetCount)
	}
	// All fast targets must have backend_kind == "mdadm".
	for _, tgt := range fastDetail.Targets {
		if tgt.BackendKind != "mdadm" {
			t.Errorf("fast domain target %q has backend_kind %q, want mdadm", tgt.Name, tgt.BackendKind)
		}
	}

	// GET /api/tiering/domains/archive must have 1 target with backend_kind == "zfsmgd".
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/domains/archive", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /domains/archive: %d", rec.Code)
	}
	var archiveDetail struct {
		TargetCount int                  `json:"target_count"`
		Targets     []tierTargetResponse `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&archiveDetail); err != nil {
		t.Fatalf("decode archive domain: %v", err)
	}
	if archiveDetail.TargetCount != 1 {
		t.Fatalf("archive domain target_count = %d, want 1", archiveDetail.TargetCount)
	}
	if archiveDetail.Targets[0].BackendKind != "zfsmgd" {
		t.Errorf("archive target backend_kind = %q, want zfsmgd", archiveDetail.Targets[0].BackendKind)
	}
}

// ---- degraded-state domain badge count --------------------------------------

// TestTieringDomainDegradedBadgeCount verifies that degraded-state entries for
// targets in a domain can be enumerated for the domain header badge.
func TestTieringDomainDegradedBadgeCount(t *testing.T) {
	h := newTieringTestHandler(t)

	tgt := &db.TierTargetRow{
		Name:            "hot-tier",
		PlacementDomain: "fast",
		BackendKind:     "mdadm",
		Rank:            1,
		Health:          "degraded",
	}
	if err := h.store.CreateTierTarget(tgt); err != nil {
		t.Fatalf("CreateTierTarget: %v", err)
	}

	// Add two degraded states for that target.
	for i, state := range []*db.DegradedStateRow{
		{BackendKind: "mdadm", ScopeKind: "target", ScopeID: tgt.ID, Severity: "critical", Code: "array_degraded", Message: "RAID degraded"},
		{BackendKind: "mdadm", ScopeKind: "target", ScopeID: tgt.ID, Severity: "warning", Code: "spare_missing", Message: "No spare available"},
	} {
		if err := h.store.UpsertDegradedState(state); err != nil {
			t.Fatalf("UpsertDegradedState[%d]: %v", i, err)
		}
	}

	// GET /api/tiering/degraded must return both states for the target.
	rec := doRequest(t, h, http.MethodGet, "/api/tiering/degraded", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /degraded: %d", rec.Code)
	}
	var list []degradedStateResponse
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode degraded: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 degraded states, got %d", len(list))
	}
	hasCritical := false
	for _, d := range list {
		if d.Severity == "critical" && d.Code == "array_degraded" {
			hasCritical = true
		}
	}
	if !hasCritical {
		t.Error("expected a critical degraded state with code array_degraded")
	}
}

// ---- movement cancel --------------------------------------------------------

func TestTieringMovementCancel(t *testing.T) {
	t.Skip("movement_jobs dropped in migration 52")
	h := newTieringTestHandler(t)

	// Seed domain and two targets (same domain, required for movement jobs).
	src := &db.TierTargetRow{Name: "src", PlacementDomain: "dom", BackendKind: "mdadm", Rank: 1}
	dst := &db.TierTargetRow{Name: "dst", PlacementDomain: "dom", BackendKind: "mdadm", Rank: 2}
	if err := h.store.CreateTierTarget(src); err != nil {
		t.Fatalf("CreateTierTarget src: %v", err)
	}
	if err := h.store.CreateTierTarget(dst); err != nil {
		t.Fatalf("CreateTierTarget dst: %v", err)
	}

	// Create a namespace so movement jobs have a valid namespace_id.
	ns := &db.ManagedNamespaceRow{
		Name:            "vol1",
		PlacementDomain: "dom",
		BackendKind:     "mdadm",
		NamespaceKind:   "volume",
		PinState:        "none",
		Health:          "healthy",
		PlacementState:  "placed",
	}
	if err := h.store.CreateManagedNamespace(ns); err != nil {
		t.Fatalf("CreateManagedNamespace: %v", err)
	}

	job := &db.MovementJobRow{
		BackendKind:     "mdadm",
		NamespaceID:     ns.ID,
		PlacementDomain: "dom",
		SourceTargetID:  src.ID,
		DestTargetID:    dst.ID,
		MovementUnit:    "region",
		State:           db.MovementJobStateRunning,
		TriggeredBy:     "policy",
	}
	if err := h.store.CreateMovementJob(job); err != nil {
		t.Fatalf("CreateMovementJob: %v", err)
	}

	// List movements must include the running job.
	rec := doRequest(t, h, http.MethodGet, "/api/tiering/movements", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /movements: %d", rec.Code)
	}
	var jobs []movementJobResponse
	if err := json.NewDecoder(rec.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode movements: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 movement job, got %d", len(jobs))
	}
	if jobs[0].State != "running" {
		t.Fatalf("state = %q, want running", jobs[0].State)
	}

	// Cancel the job.
	rec = doRequest(t, h, http.MethodDelete, "/api/tiering/movements/"+job.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /movements/%s: %d; body=%s", job.ID, rec.Code, rec.Body)
	}

	// List movements: job must now be cancelled.
	rec = doRequest(t, h, http.MethodGet, "/api/tiering/movements", nil)
	if err := json.NewDecoder(rec.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode movements after cancel: %v", err)
	}
	if jobs[0].State != "cancelled" {
		t.Fatalf("state after cancel = %q, want cancelled", jobs[0].State)
	}
}

// ---- meta store endpoints ---------------------------------------------------
//
// TestMetaStatsEndpoint and TestListNamespaceFilesEndpoint live in
// tiering_meta_endpoints_test.go (in the same package). That file imports
// meta and mdadm adapter types, which this file avoids to keep the main
// stub narrow.
