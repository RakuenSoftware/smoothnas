package compose

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Meta is the SmoothNAS display + UI-embed metadata a plain compose plugin
// carries in its top-level `x-smoothnas:` extension block. Compose ignores
// top-level `x-*` keys (compose-spec / docker compose v2), so a manifest with
// this block is still a valid plain compose file that `docker compose up` runs
// unchanged; tierd reads the extras for the catalog UI and the console embed.
//
// x-smoothnas is kept deliberately narrow: catalog display + UI embed only
// (plus the pre-existing tier/secrets extensions). Runtime concerns — images,
// ports, volumes, devices, GPU reservations, commands — stay in standard
// compose, so a converted plugin never becomes a second native manifest.
type Meta struct {
	Description string
	Vendor      string
	Homepage    string
	UI          *MetaUI // nil = headless (no console iframe; catalog shows description only)
}

// MetaUI selects and describes the service whose HTTP UI the SmoothNAS console
// embeds. Service is REQUIRED when a UI block is present and must name a key in
// the compose `services:` map — a multi-service plugin (aimee: server+kb+
// postgres) can't be disambiguated by port alone. Port is the CONTAINER-side
// port the service listens on (not a published host port); tierd builds the
// iframe src from the discovered service endpoint at runtime.
type MetaUI struct {
	Service string
	Port    int
	Path    string
	Auth    string // embed auth mode (e.g. "bearer-injected"); how the console wires auth
}

// xsDoc mirrors just the top-level x-smoothnas block we read here. Other x-
// keys (tier lives on volumes; secrets is read by SecretKeys) are untouched.
type xsDoc struct {
	XS *struct {
		Description string `yaml:"description"`
		Vendor      string `yaml:"vendor"`
		Homepage    string `yaml:"homepage"`
		UI          *struct {
			Service string `yaml:"service"`
			Port    int    `yaml:"port"`
			Path    string `yaml:"path"`
			Auth    string `yaml:"auth"`
		} `yaml:"ui"`
	} `yaml:"x-smoothnas"`
}

// ParseMeta reads the top-level x-smoothnas display/UI metadata. A manifest with
// no x-smoothnas block (or no metadata fields) parses to a zero Meta with no
// error — the block is optional and absence is tolerated gracefully.
func ParseMeta(composeYAML []byte) (Meta, error) {
	var doc xsDoc
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return Meta{}, fmt.Errorf("compose: parse x-smoothnas metadata: %w", err)
	}
	if doc.XS == nil {
		return Meta{}, nil
	}
	m := Meta{Description: doc.XS.Description, Vendor: doc.XS.Vendor, Homepage: doc.XS.Homepage}
	if doc.XS.UI != nil {
		m.UI = &MetaUI{Service: doc.XS.UI.Service, Port: doc.XS.UI.Port, Path: doc.XS.UI.Path, Auth: doc.XS.UI.Auth}
	}
	return m, nil
}

// ServiceNames returns the keys of the compose `services:` map, so callers can
// validate references (e.g. x-smoothnas.ui.service) at catalog-ingest time —
// compose ignores x-* entirely, so a mis-spelled service would otherwise fail
// silently at runtime rather than loudly at publish.
func ServiceNames(composeYAML []byte) ([]string, error) {
	var doc struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse services: %w", err)
	}
	names := make([]string, 0, len(doc.Services))
	for k := range doc.Services {
		names = append(names, k)
	}
	return names, nil
}

// ValidateMeta enforces the UI-embed contract: when a ui block is present its
// service is required and must name a real compose service, its port must be a
// valid TCP port, and its path (if set) must be absolute. Returns nil for a
// headless plugin (no ui block).
func ValidateMeta(m Meta, serviceNames []string) error {
	if m.UI == nil {
		return nil
	}
	if m.UI.Service == "" {
		return fmt.Errorf("x-smoothnas.ui.service is required when x-smoothnas.ui is set")
	}
	found := false
	for _, n := range serviceNames {
		if n == m.UI.Service {
			found = true
			break
		}
	}
	if !found {
		sorted := append([]string(nil), serviceNames...)
		sort.Strings(sorted) // deterministic message for logs/snapshots
		return fmt.Errorf("x-smoothnas.ui.service %q does not name a compose service (have %v)", m.UI.Service, sorted)
	}
	if m.UI.Port < 1 || m.UI.Port > 65535 {
		return fmt.Errorf("x-smoothnas.ui.port %d out of range (want 1..65535)", m.UI.Port)
	}
	if m.UI.Path != "" && !strings.HasPrefix(m.UI.Path, "/") {
		return fmt.Errorf("x-smoothnas.ui.path %q must be absolute (start with /)", m.UI.Path)
	}
	return nil
}

// RejectMultiDoc returns an error if the bytes hold more than one YAML document
// (`---`-separated). A docker-compose file is a single document; our parsers
// (ParseMeta, ServiceNames) only read the first, so a multi-doc file would
// silently drop services/metadata. Fail loudly at ingest instead.
func RejectMultiDoc(b []byte) error {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil // empty input: let the format detector handle it
		}
		return fmt.Errorf("compose: parse: %w", err)
	}
	if err := dec.Decode(&doc); err != io.EOF {
		if err == nil {
			return fmt.Errorf("compose: multi-document YAML is not a valid compose file")
		}
		return fmt.Errorf("compose: parse: %w", err)
	}
	return nil
}
