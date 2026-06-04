package plugin

import (
	"strings"
	"testing"
)

// TestAimeeManifests_ParseValidate proves the published compose-style aimee
// plugin manifests (RakuenSoftware/smoothnas-plugin-aimee) parse and validate
// against the smoothnas.io/v1 services schema, and that aimee-kb / aimee-combined
// bundle Postgres + the embedder as health-gated dependencies of the app
// service — i.e. no external database is required.
func TestAimeeManifests_ParseValidate(t *testing.T) {
	cases := []struct {
		file         string
		name         string
		wantServices []string
		// app is the service whose env wires discovery to the backends.
		app            string
		wantHostTokens []string
	}{
		{
			file:           "aimee-server.yaml",
			name:           "aimee-server",
			wantServices:   []string{"aimee-server"},
			app:            "aimee-server",
			wantHostTokens: nil, // standalone: no bundled backends
		},
		{
			file:           "aimee-kb.yaml",
			name:           "aimee-kb",
			wantServices:   []string{"postgres", "embedder", "kb"},
			app:            "kb",
			wantHostTokens: []string{"{{service.postgres.host}}", "{{service.embedder.host}}"},
		},
		{
			file:           "aimee-combined.yaml",
			name:           "aimee-combined",
			wantServices:   []string{"postgres", "embedder", "server-kb"},
			app:            "server-kb",
			wantHostTokens: []string{"{{service.postgres.host}}", "{{service.embedder.host}}"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.file, func(t *testing.T) {
			m := mustParse(t, tc.file)
			if m.Metadata.Name != tc.name {
				t.Fatalf("name = %q want %q", m.Metadata.Name, tc.name)
			}

			got := map[string]*Service{}
			for i := range m.Services {
				got[m.Services[i].Name] = &m.Services[i]
			}
			if len(got) != len(tc.wantServices) {
				t.Fatalf("services = %d want %d", len(got), len(tc.wantServices))
			}
			for _, name := range tc.wantServices {
				if got[name] == nil {
					t.Fatalf("missing service %q; have %v", name, serviceNames(m))
				}
			}

			app := got[tc.app]
			// The app's discovery env must reference each backend host token.
			joined := strings.Join(envValues(app), "\n")
			for _, tok := range tc.wantHostTokens {
				if !strings.Contains(joined, tok) {
					t.Errorf("app %q env missing discovery token %q; env=%q", tc.app, tok, joined)
				}
			}

			// When the app depends on backends, those deps must be
			// service_healthy and the targets must declare a health block —
			// ValidateManifest enforces this, so a clean validate proves it.
			for dep, cond := range app.DependsOn {
				if cond.Condition != DependsServiceHealthy {
					t.Errorf("app %q dependsOn %q condition = %q want service_healthy", tc.app, dep, cond.Condition)
				}
				if got[dep] == nil || got[dep].Health == nil {
					t.Errorf("dependency %q must declare a health block", dep)
				}
			}
		})
	}
}

func serviceNames(m *Manifest) []string {
	out := make([]string, 0, len(m.Services))
	for i := range m.Services {
		out = append(out, m.Services[i].Name)
	}
	return out
}

func envValues(s *Service) []string {
	out := make([]string, 0, len(s.Env))
	for _, v := range s.Env {
		out = append(out, v)
	}
	return out
}
