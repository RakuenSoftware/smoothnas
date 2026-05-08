package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
)

// ProfileHandler exposes the plugin profile catalog over HTTP. Read-
// only — operators add custom profiles by dropping YAML files into
// /etc/smoothnas/plugin-profiles.d/ and the daemon picks them up on
// catalog reload (manual today; phase 06 might add a watcher).
type ProfileHandler struct {
	catalog *plugin.Catalog
}

// NewProfileHandler constructs a ProfileHandler around an already-
// loaded catalog. Pass plugin.NewCatalog(plugin.DefaultOperatorProfilesDir).
func NewProfileHandler(c *plugin.Catalog) *ProfileHandler {
	return &ProfileHandler{catalog: c}
}

// Route dispatches on path + method.
//
//	GET  /api/plugin-profiles            → list
//	GET  /api/plugin-profiles/<name>     → show one
//	POST /api/plugin-profiles/preview    → preview a manifest's resolved fragments
func (h *ProfileHandler) Route(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/plugin-profiles"
	path := strings.TrimPrefix(r.URL.Path, prefix)

	switch {
	case path == "" || path == "/":
		if r.Method != http.MethodGet {
			jsonMethodNotAllowed(w)
			return
		}
		h.list(w, r)
	case path == "/preview":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.preview(w, r)
	case strings.HasPrefix(path, "/"):
		if r.Method != http.MethodGet {
			jsonMethodNotAllowed(w)
			return
		}
		h.show(w, r, strings.TrimPrefix(path, "/"))
	default:
		jsonNotFound(w)
	}
}

// listResponse is the GET / shape.
type profileListEntry struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Description string `json:"description"`
}

func (h *ProfileHandler) list(w http.ResponseWriter, _ *http.Request) {
	out := []profileListEntry{}
	for _, p := range h.catalog.List() {
		out = append(out, profileListEntry{
			Name:        p.Metadata.Name,
			Source:      p.Source,
			Description: p.Metadata.Description,
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"profiles": out})
}

func (h *ProfileHandler) show(w http.ResponseWriter, _ *http.Request, name string) {
	p, ok := h.catalog.Get(name)
	if !ok {
		jsonErrorCoded(w, "profile not found", http.StatusNotFound, "plugin_profiles.not_found")
		return
	}
	_ = json.NewEncoder(w).Encode(p)
}

// previewRequest is the POST /preview input. Operators paste the
// manifest YAML they intend to install; the response is what
// Resolve() would produce, so the install wizard can show the
// resolved devices/env/limits before committing.
type previewRequest struct {
	Manifest string `json:"manifest"`
}

// previewResponse mirrors plugin.Resolved for JSON.
type previewResponse struct {
	Names             []string                 `json:"names"`
	Devices           []plugin.ProfileDevice   `json:"devices,omitempty"`
	CapAdd            []string                 `json:"capAdd,omitempty"`
	PidsLimit         int64                    `json:"pidsLimit,omitempty"`
	OomScoreAdj       *int                     `json:"oomScoreAdj,omitempty"`
	Env               map[string]string        `json:"env,omitempty"`
	LXCRaw            []string                 `json:"lxcRaw,omitempty"`
	PreflightWarnings []string                 `json:"preflightWarnings,omitempty"`
}

func (h *ProfileHandler) preview(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Manifest == "" {
		jsonErrorCoded(w, "manifest is required", http.StatusBadRequest, "plugin_profiles.manifest_missing")
		return
	}
	m, err := plugin.ParseManifest([]byte(req.Manifest))
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugin_profiles.manifest_parse")
		return
	}
	if err := plugin.ValidateManifest(m); err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugin_profiles.manifest_invalid")
		return
	}
	resolved, err := plugin.Resolve(h.catalog, m, nil)
	if err != nil {
		// Per the proposal, required-preflight failures and missing
		// profiles surface here; treat both as 400 since the operator
		// can fix them by editing the manifest or installing the
		// profile.
		var pe *plugin.PreflightError
		if errors.As(err, &pe) {
			jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugin_profiles.preflight")
			return
		}
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "plugin_profiles.resolve")
		return
	}
	_ = json.NewEncoder(w).Encode(previewResponse{
		Names:             resolved.Names,
		Devices:           resolved.Devices,
		CapAdd:            resolved.CapAdd,
		PidsLimit:         resolved.PidsLimit,
		OomScoreAdj:       resolved.OomScoreAdj,
		Env:               resolved.Env,
		LXCRaw:            resolved.LXCRaw,
		PreflightWarnings: resolved.PreflightWarnings,
	})
}
