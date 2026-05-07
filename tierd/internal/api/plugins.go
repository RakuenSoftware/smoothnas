package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

// PluginsHandler exposes the plugin subsystem over HTTP. Mirrors
// the surface tierd-cli already has + the SSE log/event streams the
// UI needs. Every endpoint is admin-only — the router gates that.
type PluginsHandler struct {
	store        *plugin.Store
	installer    *plugin.Installer
	lifecycle    *plugin.Lifecycle
	catalog      *plugin.Catalog
	tierProvider plugin.TierProvider
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
//	PUT    /api/plugins/<name>/config         update plugin_config
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
	case "start", "stop", "restart", "materialise", "materialize":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.lifecycleVerb(w, r, name, verb)
	case "config":
		if r.Method != http.MethodPut {
			jsonMethodNotAllowed(w)
			return
		}
		h.updateConfig(w, r, name)
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
	Name                 string   `json:"name"`
	Version              string   `json:"version"`
	State                string   `json:"state"`
	ArtifactType         string   `json:"artifactType"`
	ImageRef             string   `json:"imageRef,omitempty"`
	DistroSummary        string   `json:"distroSummary,omitempty"`
	InstanceCount        int      `json:"instanceCount"`
	InstanceConfigurable bool     `json:"instanceConfigurable"`
	ResolvedProfiles     []string `json:"resolvedProfiles"`
	InstalledAt          string   `json:"installedAt"`
	UpdatedAt            string   `json:"updatedAt"`
}

func (h *PluginsHandler) list(w http.ResponseWriter, _ *http.Request) {
	rows, err := h.store.List()
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]pluginListItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, toPluginListItem(r))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"plugins": out})
}

func toPluginListItem(r plugin.PluginRow) pluginListItem {
	profiles := r.ResolvedProfiles
	if profiles == nil {
		profiles = []string{}
	}
	return pluginListItem{
		Name:                 r.Name,
		Version:              r.Version,
		State:                r.State,
		ArtifactType:         r.ArtifactType,
		ImageRef:             r.ImageRef,
		DistroSummary:        r.DistroSummary,
		InstanceCount:        r.InstanceCount,
		InstanceConfigurable: r.InstanceConfigurable,
		ResolvedProfiles:     profiles,
		InstalledAt:          r.InstalledAt,
		UpdatedAt:            r.UpdatedAt,
	}
}

// detail returns the full PluginRecord (plugin row + per-instance
// state + volumes + ports + config) for one plugin.
type pluginDetail struct {
	Plugin    pluginListItem        `json:"plugin"`
	Instances []plugin.InstanceRow  `json:"instances"`
	Volumes   []plugin.VolumeRow    `json:"volumes"`
	Ports     []plugin.PortRow      `json:"ports"`
	Config    []plugin.ConfigRow    `json:"config"`
	Manifest  string                `json:"manifest"`
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
	_ = json.NewEncoder(w).Encode(pluginDetail{
		Plugin:    toPluginListItem(rec.Plugin),
		Instances: rec.Instances,
		Volumes:   rec.Volumes,
		Ports:     rec.Ports,
		Config:    rec.Config,
		Manifest:  rec.Plugin.ManifestYAML,
	})
}

// preflightRequest is the body of POST /api/plugins/preflight.
// `manifest` is the raw YAML the wizard collected at step 1.
// `tierAssignments` mirrors plugin.TierAssignments — operator's
// choices for tier-bound volumes.
type preflightRequest struct {
	Manifest        string                 `json:"manifest"`
	TierAssignments preflightTierAssign    `json:"tierAssignments"`
}

type preflightTierAssign struct {
	Default   string            `json:"default"`
	PerVolume map[string]string `json:"perVolume"`
}

// preflightResponse mirrors plugin.PreflightResult for JSON output.
type preflightResponse struct {
	OK         bool                       `json:"ok"`
	Placements []plugin.VolumePlacement   `json:"placements"`
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
// the preflight request shape.
type installRequest struct {
	Manifest        string              `json:"manifest"`
	TierAssignments preflightTierAssign `json:"tierAssignments"`
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
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(pluginDetail{
		Plugin:    toPluginListItem(rec.Plugin),
		Instances: rec.Instances,
		Volumes:   rec.Volumes,
		Ports:     rec.Ports,
		Config:    rec.Config,
		Manifest:  rec.Plugin.ManifestYAML,
	})
}

// lifecycleVerb dispatches start/stop/restart/materialise.
func (h *PluginsHandler) lifecycleVerb(w http.ResponseWriter, r *http.Request, name, verb string) {
	if h.lifecycle == nil {
		jsonErrorCoded(w, "runtime not configured", http.StatusServiceUnavailable, "plugins.runtime_unavailable")
		return
	}
	ctx := r.Context()
	var err error
	switch verb {
	case "materialise", "materialize":
		err = h.lifecycle.Materialise(ctx, name)
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

