package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/gpu"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

// detachRequest returns a context derived from the request that is NOT
// cancelled when the HTTP client goes away (a closed connection or, in front
// of tierd, an nginx proxy_read_timeout). Long-running, mutating lifecycle
// operations — install/update materialise+start, the lifecycle verbs, and
// scale — must run to completion even when the caller has stopped waiting:
// otherwise the disconnect cancels the operation mid-flight and leaves the
// plugin in a partial/degraded state (e.g. images pulled but containers never
// created, or some services up and others "created"). Request-scoped values
// are preserved; only cancellation and any deadline are dropped. Read-only and
// streaming handlers keep using r.Context() so they still abort on disconnect.
func detachRequest(r *http.Request) context.Context {
	return context.WithoutCancel(r.Context())
}

// PluginsHandler exposes the plugin subsystem over HTTP. Mirrors
// the surface tierd-cli already has + the SSE log/event streams the
// UI needs. Every endpoint is admin-only — the router gates that.
type PluginsHandler struct {
	store        *plugin.Store
	installer    *plugin.Installer
	lifecycle    *plugin.Lifecycle
	catalog      *plugin.Catalog
	tierProvider plugin.TierProvider

	catalogHTTPClient *http.Client
	catalogAPIBaseURL string
}

// NewPluginsHandler constructs a handler from the already-wired
// store / installer / lifecycle / catalog / tierProvider. The router
// is responsible for sharing those instances with other plugin-aware
// handlers (the tier-deletion blocker etc.) so there is exactly one
// Lifecycle reading and writing plugin_instances.
//
// tierProvider is the same TierProvider the installer is wired with
// — preflight has to consult the same source of tier truth or the
// preview won't match the install.
func NewPluginsHandler(
	store *plugin.Store,
	installer *plugin.Installer,
	lc *plugin.Lifecycle,
	cat *plugin.Catalog,
	tp plugin.TierProvider,
) *PluginsHandler {
	return &PluginsHandler{
		store:        store,
		installer:    installer,
		lifecycle:    lc,
		catalog:      cat,
		tierProvider: tp,
	}
}

// Route dispatches on path + method per the proposal's table:
//
//	GET    /api/plugins                       list
//	POST   /api/plugins/preflight             preflight a manifest
//	POST   /api/plugins/install               install (manifest URL or body)
//	GET    /api/plugins/<name>                detail
//	DELETE /api/plugins/<name>                full uninstall
//	POST   /api/plugins/<name>/start          lifecycle
//	POST   /api/plugins/<name>/stop           lifecycle
//	POST   /api/plugins/<name>/restart        lifecycle
//	POST   /api/plugins/<name>/materialise    pull image + create containers
//	POST   /api/plugins/<name>/refresh-containers pull container refs + recreate changed containers
//	POST   /api/plugins/<name>/update         replace manifest + materialise
//	POST   /api/plugins/<name>/models/install download model, set MODEL_PATH, start
//	PUT    /api/plugins/<name>/config         update plugin_config
//	GET    /api/plugins/<name>/instances      per-instance state (phase 09)
//	POST   /api/plugins/<name>/instances      scale to {count: N}    (phase 09)
//	GET    /api/plugins/<name>/logs           SSE stream
//	GET    /api/plugins/<name>/events         SSE stream
func (h *PluginsHandler) Route(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/plugins"
	path := strings.TrimPrefix(r.URL.Path, prefix)

	switch {
	case path == "" || path == "/":
		if r.Method == http.MethodGet {
			h.list(w, r)
			return
		}
		jsonMethodNotAllowed(w)
	case path == "/preflight":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.preflight(w, r)
	case path == "/parse":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.parse(w, r)
	case path == "/catalog/latest":
		if r.Method != http.MethodGet {
			jsonMethodNotAllowed(w)
			return
		}
		h.catalogLatest(w, r)
	case path == "/gpus":
		if r.Method != http.MethodGet {
			jsonMethodNotAllowed(w)
			return
		}
		h.listGPUs(w, r)
	case path == "/install":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.install(w, r)
	case strings.HasPrefix(path, "/"):
		h.routeNamed(w, r, strings.TrimPrefix(path, "/"))
	default:
		jsonNotFound(w)
	}
}

// routeNamed handles every endpoint scoped to a specific plugin
// name. Splits "<name>" / "<name>/<verb>" once and dispatches.
func (h *PluginsHandler) routeNamed(w http.ResponseWriter, r *http.Request, rest string) {
	name, verb := splitNameVerb(rest)
	if name == "" {
		jsonNotFound(w)
		return
	}

	switch verb {
	case "":
		switch r.Method {
		case http.MethodGet:
			h.detail(w, r, name)
		case http.MethodDelete:
			h.uninstall(w, r, name)
		default:
			jsonMethodNotAllowed(w)
		}
	case "start", "stop", "restart", "materialise", "materialize", "refresh-containers":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.lifecycleVerb(w, r, name, verb)
	case "update":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.update(w, r, name)
	case "models/install":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.installModel(w, r, name)
	case "rotate-token":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.rotateToken(w, r, name)
	case "config":
		if r.Method != http.MethodPut {
			jsonMethodNotAllowed(w)
			return
		}
		h.updateConfig(w, r, name)
	case "image":
		if r.Method != http.MethodPut {
			jsonMethodNotAllowed(w)
			return
		}
		h.setPinnedImage(w, r, name)
	case "instances":
		switch r.Method {
		case http.MethodGet:
			h.listInstances(w, r, name)
		case http.MethodPost:
			h.scaleInstances(w, r, name)
		default:
			jsonMethodNotAllowed(w)
		}
	case "logs":
		if r.Method != http.MethodGet {
			jsonMethodNotAllowed(w)
			return
		}
		h.streamLogs(w, r, name)
	case "events":
		if r.Method != http.MethodGet {
			jsonMethodNotAllowed(w)
			return
		}
		h.streamEvents(w, r, name)
	default:
		jsonNotFound(w)
	}
}

func (h *PluginsHandler) listGPUs(w http.ResponseWriter, _ *http.Request) {
	devices, err := gpu.List()
	if err != nil {
		serverError(w, err)
		return
	}
	if devices == nil {
		devices = []gpu.Device{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"gpus": devices})
}

// manifestNameForDuplicateCheck does a tolerant parse of just the
// metadata.name field so the install handler can fast-fail with a
// 409 before running the more expensive preflight + filesystem
// pipeline. Returns ("", err) on parse failure — caller falls
// through to the regular install path which will surface the parse
// error properly.
func manifestNameForDuplicateCheck(yamlText string) (string, error) {
	m, err := plugin.ParseManifest([]byte(yamlText))
	if err != nil {
		return "", err
	}
	return m.Metadata.Name, nil
}

// splitNameVerb takes "<name>" or "<name>/<verb>" and returns the two
// parts. Verb is "" when there is no slash.
func splitNameVerb(rest string) (name, verb string) {
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

// list returns the lightweight plugins-table view used by the UI's
// list page. Wraps each row in the JSON shape the wizard expects
// (the raw DB row is exposed under .plugin so future fields don't
// need a UI churn).
type pluginListItem struct {
	Name                     string                   `json:"name"`
	Version                  string                   `json:"version"`
	State                    string                   `json:"state"`
	ArtifactType             string                   `json:"artifactType"`
	ImageRef                 string                   `json:"imageRef,omitempty"`
	DistroSummary            string                   `json:"distroSummary,omitempty"`
	InstanceCount            int                      `json:"instanceCount"`
	InstanceConfigurable     bool                     `json:"instanceConfigurable"`
	ResolvedProfiles         []string                 `json:"resolvedProfiles"`
	ContainerRefs            []pluginContainerRefItem `json:"containerRefs"`
	ContainerUpdateAvailable bool                     `json:"containerUpdateAvailable"`
	InstalledAt              string                   `json:"installedAt"`
	UpdatedAt                string                   `json:"updatedAt"`
}

type pluginContainerRefItem struct {
	Service     string `json:"service"`
	Name        string `json:"name"`
	ImageRef    string `json:"imageRef"`
	ResolvedRef string `json:"resolvedRef,omitempty"`
	Digest      string `json:"digest,omitempty"`
	UpdatedAt   string `json:"updatedAt"`
}

func (h *PluginsHandler) list(w http.ResponseWriter, _ *http.Request) {
	rows, err := h.store.List()
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]pluginListItem, 0, len(rows))
	for _, r := range rows {
		rec, err := h.store.Get(r.Name)
		if err != nil {
			serverError(w, err)
			return
		}
		out = append(out, toPluginListItem(rec))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"plugins": out})
}

func toPluginListItem(rec *plugin.PluginRecord) pluginListItem {
	r := rec.Plugin
	profiles := r.ResolvedProfiles
	if profiles == nil {
		profiles = []string{}
	}
	return pluginListItem{
		Name:                     r.Name,
		Version:                  r.Version,
		State:                    r.State,
		ArtifactType:             r.ArtifactType,
		ImageRef:                 r.ImageRef,
		DistroSummary:            r.DistroSummary,
		InstanceCount:            r.InstanceCount,
		InstanceConfigurable:     r.InstanceConfigurable,
		ResolvedProfiles:         profiles,
		ContainerRefs:            toPluginContainerRefItems(rec.ContainerRefs),
		ContainerUpdateAvailable: hasMutableContainerRef(rec.ContainerRefs),
		InstalledAt:              r.InstalledAt,
		UpdatedAt:                r.UpdatedAt,
	}
}

func toPluginContainerRefItems(refs []plugin.ContainerRefRow) []pluginContainerRefItem {
	if refs == nil {
		return []pluginContainerRefItem{}
	}
	out := make([]pluginContainerRefItem, 0, len(refs))
	for _, ref := range refs {
		out = append(out, pluginContainerRefItem{
			Service:     ref.Service,
			Name:        ref.Name,
			ImageRef:    ref.ImageRef,
			ResolvedRef: ref.ResolvedRef,
			Digest:      ref.Digest,
			UpdatedAt:   ref.UpdatedAt,
		})
	}
	return out
}

func hasMutableContainerRef(refs []plugin.ContainerRefRow) bool {
	for _, ref := range refs {
		if ref.ImageRef != "" && !strings.Contains(ref.ImageRef, "@sha256:") {
			return true
		}
	}
	return false
}

// detail returns the full PluginRecord (plugin row + per-instance
// state + volumes + ports + config) for one plugin.
type pluginDetail struct {
	Plugin        pluginListItem           `json:"plugin"`
	Services      []pluginServiceItem      `json:"services"`
	Instances     []plugin.InstanceRow     `json:"instances"`
	Volumes       []plugin.VolumeRow       `json:"volumes"`
	Ports         []plugin.PortRow         `json:"ports"`
	Config        []plugin.ConfigRow       `json:"config"`
	ContainerRefs []plugin.ContainerRefRow `json:"containerRefs"`
	Manifest      string                   `json:"manifest"`
}

// pluginServiceItem is the UI-facing view of one service in a
// compose-style plugin: its image, ordering, dependencies, whether it
// declares a healthcheck, and the rolled-up state of its instances.
type pluginServiceItem struct {
	Name          string               `json:"name"`
	ArtifactType  string               `json:"artifactType"`
	ImageRef      string               `json:"imageRef,omitempty"`
	PinnedImage   string               `json:"pinnedImage,omitempty"`
	DistroSummary string               `json:"distroSummary,omitempty"`
	Ordinal       int                  `json:"ordinal"`
	DependsOn     map[string]string    `json:"dependsOn,omitempty"`
	HasHealth     bool                 `json:"hasHealth"`
	State         string               `json:"state"`
	Instances     []plugin.InstanceRow `json:"instances"`
}

func toPluginDetail(rec *plugin.PluginRecord) pluginDetail {
	instances := rec.Instances
	if instances == nil {
		instances = []plugin.InstanceRow{}
	}
	volumes := rec.Volumes
	if volumes == nil {
		volumes = []plugin.VolumeRow{}
	}
	ports := rec.Ports
	if ports == nil {
		ports = []plugin.PortRow{}
	}
	config := rec.Config
	if config == nil {
		config = []plugin.ConfigRow{}
	}
	containerRefs := rec.ContainerRefs
	if containerRefs == nil {
		containerRefs = []plugin.ContainerRefRow{}
	}
	return pluginDetail{
		Plugin:        toPluginListItem(rec),
		Services:      toPluginServiceItems(rec),
		Instances:     instances,
		Volumes:       volumes,
		Ports:         ports,
		Config:        config,
		ContainerRefs: containerRefs,
		Manifest:      rec.Plugin.ManifestYAML,
	}
}

// toPluginServiceItems rolls the per-service rows up into the UI view,
// pairing each service with its own instances and an aggregate state.
func toPluginServiceItems(rec *plugin.PluginRecord) []pluginServiceItem {
	out := make([]pluginServiceItem, 0, len(rec.Services))
	for _, sr := range rec.Services {
		insts := []plugin.InstanceRow{}
		counts := map[string]int{}
		for _, inst := range rec.Instances {
			if inst.Service == sr.Service {
				insts = append(insts, inst)
				counts[inst.State]++
			}
		}
		out = append(out, pluginServiceItem{
			Name:          sr.Service,
			ArtifactType:  sr.ArtifactType,
			ImageRef:      sr.ImageRef,
			PinnedImage:   sr.PinnedImage,
			DistroSummary: sr.DistroSummary,
			Ordinal:       sr.Ordinal,
			DependsOn:     sr.DependsOn,
			HasHealth:     sr.Health != nil,
			State:         plugin.AggregateInstanceStates(counts, len(insts)),
			Instances:     insts,
		})
	}
	return out
}

func (h *PluginsHandler) detail(w http.ResponseWriter, _ *http.Request, name string) {
	rec, err := h.store.Get(name)
	if err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(toPluginDetail(rec))
}

// parseRequest is the body of POST /api/plugins/parse. Used by the
// install wizard's preview step — it gets back the parsed Manifest
// (volumes, ports, config, profiles, etc.) so it can render the
// later wizard steps (tier picker, config form) without duplicating
// YAML parsing client-side.
type parseRequest struct {
	Manifest string `json:"manifest"`
}

func (h *PluginsHandler) parse(w http.ResponseWriter, r *http.Request) {
	var req parseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Manifest == "" {
		jsonErrorCoded(w, "manifest is required", http.StatusBadRequest, "plugins.manifest_missing")
		return
	}
	m, err := plugin.ParseManifest([]byte(req.Manifest))
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.manifest_parse")
		return
	}
	if err := plugin.ValidateManifest(m); err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.manifest_invalid")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"manifest": m})
}

// preflightRequest is the body of POST /api/plugins/preflight.
// `manifest` is the raw YAML the wizard collected at step 1.
// `tierAssignments` mirrors plugin.TierAssignments — operator's
// choices for tier-bound volumes.
type preflightRequest struct {
	Manifest        string              `json:"manifest"`
	TierAssignments preflightTierAssign `json:"tierAssignments"`
}

type preflightTierAssign struct {
	Default   string            `json:"default"`
	PerVolume map[string]string `json:"perVolume"`
}

// preflightResponse mirrors plugin.PreflightResult for JSON output.
type preflightResponse struct {
	OK         bool                     `json:"ok"`
	Placements []plugin.VolumePlacement `json:"placements"`
}

func (h *PluginsHandler) preflight(w http.ResponseWriter, r *http.Request) {
	var req preflightRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Manifest == "" {
		jsonErrorCoded(w, "manifest is required", http.StatusBadRequest, "plugins.manifest_missing")
		return
	}
	m, err := plugin.ParseManifest([]byte(req.Manifest))
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.manifest_parse")
		return
	}
	if err := plugin.ValidateManifest(m); err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.manifest_invalid")
		return
	}
	// Use the same TierProvider the installer uses so preflight gives
	// the same answer install would.
	tp := h.tierProvider
	if tp == nil {
		tp = h.store.RawDB()
	}
	res, err := plugin.PreflightTierAssignments(
		tp, nil, m,
		plugin.TierAssignments{Default: req.TierAssignments.Default, PerVolume: req.TierAssignments.PerVolume},
		plugin.DefaultPluginsRoot,
	)
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(preflightResponse{OK: res.OK, Placements: res.Placements})
}

// installRequest is the body of POST /api/plugins/install. `manifest`
// is the raw YAML the wizard collected; `tierAssignments` mirrors
// the preflight request shape. `config` carries install-time config
// overrides that must be present before materialisation/autostart.
type installRequest struct {
	Manifest        string              `json:"manifest"`
	TierAssignments preflightTierAssign `json:"tierAssignments"`
	Config          map[string]string   `json:"config"`
}

func (h *PluginsHandler) install(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Manifest == "" {
		jsonErrorCoded(w, "manifest is required", http.StatusBadRequest, "plugins.manifest_missing")
		return
	}
	// Surface duplicate-name as 409 before preflight runs, so the
	// path-conflict gate doesn't shadow the friendlier message
	// (a leftover dir from a partial install would otherwise hide
	// "this plugin is already installed").
	if name, _ := manifestNameForDuplicateCheck(req.Manifest); name != "" {
		if _, err := h.store.Get(name); err == nil {
			jsonErrorCoded(w, "plugin already installed", http.StatusConflict, "plugins.already_exists")
			return
		}
	}
	rec, err := h.installer.InstallWithOptions([]byte(req.Manifest), plugin.InstallOptions{
		Tiers: plugin.TierAssignments{
			Default:   req.TierAssignments.Default,
			PerVolume: req.TierAssignments.PerVolume,
		},
		Config: req.Config,
	})
	if err != nil {
		// Preflight failures map to 400 with the per-volume detail.
		var pe *plugin.PreflightError
		if errors.As(err, &pe) {
			jsonErrorCoded(w, pe.Error(), http.StatusBadRequest, "plugins.preflight_failed")
			return
		}
		var ve *plugin.ValidationError
		if errors.As(err, &ve) {
			jsonErrorCoded(w, ve.Error(), http.StatusBadRequest, "plugins.manifest_invalid")
			return
		}
		if errors.Is(err, plugin.ErrPluginExists) {
			jsonErrorCoded(w, "plugin already installed", http.StatusConflict, "plugins.already_exists")
			return
		}
		serverError(w, err)
		return
	}
	if h.lifecycle != nil {
		opCtx := detachRequest(r)
		if err := h.lifecycle.Materialise(opCtx, rec.Plugin.Name); err != nil {
			jsonErrorCoded(w, fmt.Sprintf("autostart materialise: %v", err), http.StatusInternalServerError, "plugins.lifecycle_failed")
			return
		}
		if err := h.lifecycle.Start(opCtx, rec.Plugin.Name); err != nil {
			jsonErrorCoded(w, fmt.Sprintf("autostart start: %v", err), http.StatusInternalServerError, "plugins.lifecycle_failed")
			return
		}
		rec, err = h.store.Get(rec.Plugin.Name)
		if err != nil {
			serverError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toPluginDetail(rec))
}

func (h *PluginsHandler) update(w http.ResponseWriter, r *http.Request, name string) {
	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Manifest == "" {
		jsonErrorCoded(w, "manifest is required", http.StatusBadRequest, "plugins.manifest_missing")
		return
	}
	m, err := plugin.ParseManifest([]byte(req.Manifest))
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.manifest_parse")
		return
	}
	if err := plugin.ValidateManifest(m); err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.manifest_invalid")
		return
	}
	if m.Metadata.Name != name {
		jsonErrorCoded(w, "manifest name does not match installed plugin", http.StatusBadRequest, "plugins.manifest_name_mismatch")
		return
	}

	rec, err := h.store.Get(name)
	if err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}
	wasRunning := rec.Plugin.State == plugin.StateRunning

	if err := h.store.UpdateManifest(name, m, req.Manifest); err != nil {
		switch {
		case errors.Is(err, plugin.ErrPluginNotFound):
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
		case errors.Is(err, plugin.ErrPluginUpdateRequiresReinstall):
			jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.update_requires_reinstall")
		default:
			serverError(w, err)
		}
		return
	}

	if h.lifecycle != nil {
		opCtx := detachRequest(r)
		if err := h.lifecycle.Materialise(opCtx, name); err != nil {
			jsonErrorCoded(w, fmt.Sprintf("update materialise: %v", err), http.StatusInternalServerError, "plugins.lifecycle_failed")
			return
		}
		if wasRunning {
			if err := h.lifecycle.Start(opCtx, name); err != nil {
				jsonErrorCoded(w, fmt.Sprintf("update start: %v", err), http.StatusInternalServerError, "plugins.lifecycle_failed")
				return
			}
		}
	}
	rec, err = h.store.Get(name)
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(toPluginDetail(rec))
}

// lifecycleVerb dispatches start/stop/restart/materialise.
func (h *PluginsHandler) lifecycleVerb(w http.ResponseWriter, r *http.Request, name, verb string) {
	if h.lifecycle == nil {
		jsonErrorCoded(w, "runtime not configured", http.StatusServiceUnavailable, "plugins.runtime_unavailable")
		return
	}
	// Detached so a client/proxy disconnect can't cancel a materialise or
	// restart partway through and strand the plugin in a partial state.
	ctx := detachRequest(r)
	var err error
	switch verb {
	case "materialise", "materialize":
		err = h.lifecycle.Materialise(ctx, name)
	case "refresh-containers":
		err = h.lifecycle.RefreshContainers(ctx, name)
	case "start":
		err = h.lifecycle.Start(ctx, name)
	case "stop":
		err = h.lifecycle.Stop(ctx, name)
	case "restart":
		err = h.lifecycle.Restart(ctx, name)
	}
	if err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		jsonErrorCoded(w, err.Error(), http.StatusInternalServerError, "plugins.lifecycle_failed")
		return
	}
	// Re-fetch and return the updated state so the UI's status pill
	// can update in one round-trip.
	rec, err := h.store.Get(name)
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":  name,
		"state": rec.Plugin.State,
	})
}

// updateConfig replaces the plugin_config rows with the supplied
// map and signals that a restart is needed for the new values to
// take effect (containers read env at start time).
type updateConfigRequest struct {
	Config map[string]string `json:"config"`
}

func (h *PluginsHandler) updateConfig(w http.ResponseWriter, r *http.Request, name string) {
	var req updateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	// Verify the plugin exists first so a typo'd name doesn't
	// silently update zero rows.
	if _, err := h.store.Get(name); err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}
	if err := h.store.ReplaceConfig(name, req.Config); err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":          name,
		"restartNeeded": true,
	})
}

// setPinnedImage sets (or clears, when image is empty) the operator image pin on a
// plugin's primary service. The pin is resolved and run in place of the manifest's
// primary container ref and persists across updates/reconciles (materialise re-applies
// it).
//
// Setting/clearing the pin re-materialises the plugin so the new image takes effect
// immediately (a plain restart does NOT re-resolve container refs, so it would keep
// running the old image). If the plugin was running it is restarted onto the new
// image; if it was stopped the pin is resolved/pulled but the plugin is left stopped.
type setPinnedImageRequest struct {
	Image string `json:"image"`
}

func (h *PluginsHandler) setPinnedImage(w http.ResponseWriter, r *http.Request, name string) {
	var req setPinnedImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	rec, err := h.store.Get(name)
	if err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}
	image := strings.TrimSpace(req.Image)
	if err := h.store.SetPinnedImage(name, image); err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}

	// Apply the pin in place. resolveContainerRefs only runs on materialise, so without
	// this the new image would not take effect until the next update/reconcile (a plain
	// restart reuses the already-resolved image_ref). Detached so a client disconnect
	// can't strand the plugin mid-swap.
	applied := false
	if h.lifecycle != nil {
		opCtx := detachRequest(r)
		wasRunning := rec.Plugin.State == plugin.StateRunning
		if err := h.lifecycle.Materialise(opCtx, name); err != nil {
			jsonErrorCoded(w, fmt.Sprintf("apply image pin (materialise): %v", err), http.StatusInternalServerError, "plugins.lifecycle_failed")
			return
		}
		if wasRunning {
			if err := h.lifecycle.Start(opCtx, name); err != nil {
				jsonErrorCoded(w, fmt.Sprintf("apply image pin (start): %v", err), http.StatusInternalServerError, "plugins.lifecycle_failed")
				return
			}
		}
		applied = true
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":    name,
		"image":   image,
		"applied": applied,
	})
}

// rotateToken issues a new bearer token for a plugin and re-applies
// the nginx route so the new value takes effect immediately. Returns
// the new token to the operator (one-time-only — tierd does not
// expose it again on subsequent reads).
//
// Returns 503 when no Lifecycle is wired (re-applying the route
// requires the runtime client because it captures the bridge IP);
// 404 when the plugin doesn't exist; 200 with the new token on
// success.
func (h *PluginsHandler) rotateToken(w http.ResponseWriter, r *http.Request, name string) {
	if h.lifecycle == nil {
		jsonErrorCoded(w, "runtime not configured", http.StatusServiceUnavailable, "plugins.runtime_unavailable")
		return
	}
	if _, err := h.store.Get(name); err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}
	token, err := h.store.IssueBearerToken(name)
	if err != nil {
		serverError(w, err)
		return
	}
	// Re-apply the nginx route so the new token is in the live
	// proxy config. ApplyRouteFor hides the buildPluginRoute +
	// proxy.Apply round-trip behind one method on Lifecycle.
	if err := h.lifecycle.ApplyRouteFor(r.Context(), name); err != nil {
		// Token is already issued; the operator needs to know the
		// route reload failed so they can investigate.
		jsonErrorCoded(w, fmt.Sprintf("token rotated but nginx reload failed: %v", err),
			http.StatusBadGateway, "plugins.proxy_reload_failed")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{
		"name":  name,
		"token": token,
	})
}

// uninstall handles DELETE. Wraps Installer.Uninstall — that owns
// the all-or-none teardown (containers + image + nginx + tier-bound
// dirs + DB rows) per the parent doc's policy.
func (h *PluginsHandler) uninstall(w http.ResponseWriter, _ *http.Request, name string) {
	if err := h.installer.Uninstall(name); err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
}

// streamLogs and streamEvents are SSE shells that the UI subscribes
// to. Phase 6a wires the endpoints with real source plumbing for
// logs (lifecycle exposes the runtime client's StreamLogs); events
// are stubbed until phase 02's reconciler exposes its event channel
// for fan-out (a small follow-up — kept here so the URL shape is
// stable).
func (h *PluginsHandler) streamLogs(w http.ResponseWriter, r *http.Request, name string) {
	if h.lifecycle == nil {
		jsonErrorCoded(w, "runtime not configured", http.StatusServiceUnavailable, "plugins.runtime_unavailable")
		return
	}
	rec, err := h.store.Get(name)
	if err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}
	// v1 streams instance 1's logs only. Multi-instance plugins
	// (gh-runner) get a per-instance picker in phase 6d.
	if len(rec.Instances) == 0 || rec.Instances[0].ContainerID == "" {
		jsonErrorCoded(w, "plugin has no running container; materialise + start first",
			http.StatusConflict, "plugins.no_container")
		return
	}
	containerID := rec.Instances[0].ContainerID

	stream, err := h.lifecycle.StreamContainerLogs(r.Context(), containerID)
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadGateway, "plugins.logs_unavailable")
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer SSE
	flusher, _ := w.(http.Flusher)

	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			// SSE convention: each event prefixed with "data: " and
			// terminated with a blank line. Multi-line payloads are
			// emitted as a single block — readers treat consecutive
			// "data:" lines as one event.
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(string(buf[:n]), "\n", "\ndata: "))
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// streamEvents is a placeholder until the reconciler grows a
// fan-out channel. Returning 501 with a clear code beats a half-
// implemented stream; the URL is stable for the UI.
func (h *PluginsHandler) streamEvents(w http.ResponseWriter, _ *http.Request, _ string) {
	jsonErrorCoded(w, "event stream not yet wired (follow-up)",
		http.StatusNotImplemented, "plugins.events_not_implemented")
}

// listInstancesResponse mirrors what the UI needs for the Instances
// tab — the per-instance rows plus the aggregate count and the flag
// that tells the UI whether to render the scale slider.
type listInstancesResponse struct {
	Plugin       string               `json:"plugin"`
	Count        int                  `json:"count"`
	Configurable bool                 `json:"configurable"`
	Instances    []plugin.InstanceRow `json:"instances"`
}

// listInstances returns the per-instance state for one plugin. The
// data is already in the detail payload, but exposing a dedicated
// endpoint keeps the UI's Instances tab from having to refetch the
// whole detail (manifest + volumes + ports + config + …) just to
// render a small table.
func (h *PluginsHandler) listInstances(w http.ResponseWriter, _ *http.Request, name string) {
	rec, err := h.store.Get(name)
	if err != nil {
		if errors.Is(err, plugin.ErrPluginNotFound) {
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
			return
		}
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(listInstancesResponse{
		Plugin:       rec.Plugin.Name,
		Count:        rec.Plugin.InstanceCount,
		Configurable: rec.Plugin.InstanceConfigurable,
		Instances:    rec.Instances,
	})
}

// scaleInstancesRequest is the body of POST /api/plugins/<name>/instances.
type scaleInstancesRequest struct {
	Count int `json:"count"`
}

// scaleInstances runs Lifecycle.Scale and returns the result. Errors
// from the lifecycle map to stable codes the UI can localise:
//
//	plugins.scale.not_configurable           — manifest forbids scaling
//	plugins.scale.boundary                   — refused to cross 1↔N
//	plugins.scale.invalid_target             — target < 1 / missing
//	plugins.scale.failed                     — runtime error during scale
//	plugins.runtime_unavailable              — no Lifecycle wired
//	plugins.not_found                        — typo'd plugin name
func (h *PluginsHandler) scaleInstances(w http.ResponseWriter, r *http.Request, name string) {
	if h.lifecycle == nil {
		jsonErrorCoded(w, "runtime not configured", http.StatusServiceUnavailable, "plugins.runtime_unavailable")
		return
	}
	var req scaleInstancesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	res, err := h.lifecycle.Scale(detachRequest(r), name, req.Count)
	if err != nil {
		switch {
		case errors.Is(err, plugin.ErrPluginNotFound):
			jsonErrorCoded(w, "plugin not found", http.StatusNotFound, "plugins.not_found")
		case errors.Is(err, plugin.ErrPluginNotConfigurable):
			jsonErrorCoded(w, err.Error(), http.StatusConflict, "plugins.scale.not_configurable")
		case errors.Is(err, plugin.ErrScaleAcrossSingletonBoundary):
			jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.scale.boundary")
		case errors.Is(err, plugin.ErrScaleTargetInvalid):
			jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugins.scale.invalid_target")
		default:
			jsonErrorCoded(w, err.Error(), http.StatusInternalServerError, "plugins.scale.failed")
		}
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}
