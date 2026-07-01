package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Backend implements the plugin lifecycle for a COMPOSE-format plugin by
// driving the Adapter. It is the Phase-1 replacement for the manifest
// BuildCreatePayload path (plugins-11): Materialise writes the project files +
// pulls images, Start = `up --pull never`, Teardown = `down`, and Status reads
// `compose ps` (the source of truth). tierd's DB is a cache synced from Status.
//
// The Backend is stateless: each op reconstructs the compose Project from the
// ProjectSpec + root, so it can run after a tierd restart with no in-memory
// state. Materialise must run before Start (it writes the files Start reads).
type Backend struct {
	adapter *Adapter
	root    string // project files live under <root>/<project-name>/
}

// NewBackend returns a Backend that materialises project files under root.
func NewBackend(a *Adapter, root string) *Backend {
	return &Backend{adapter: a, root: root}
}

// ProjectSpec is a compose plugin expressed as data: the compose file(s), their
// merge order, the profiles to activate, and rendered operator config (-> .env).
// Files maps filename -> YAML content; FileOrder lists them base-first (later
// files override earlier per compose merge semantics).
type ProjectSpec struct {
	Name      string
	Files     map[string]string
	FileOrder []string
	Profiles  []string
	Env       map[string]string
	// SecretEnv is injected into the `compose up` SUBPROCESS environment only (so
	// compose resolves ${KEY} from a service's environment: block) — never written
	// to a file compose loads. Kept off Materialise/ps/config to limit exposure.
	SecretEnv map[string]string
}

// dir is the on-disk project directory.
func (b *Backend) dir(name string) string { return filepath.Join(b.root, name) }

// project builds the compose Project (absolute file paths, in order) for a spec.
// It does NOT touch disk — callers that need the files present run Materialise.
func (b *Backend) project(s ProjectSpec) Project {
	d := b.dir(s.Name)
	order := s.FileOrder
	if len(order) == 0 { // deterministic fallback if caller didn't order
		for f := range s.Files {
			order = append(order, f)
		}
		sort.Strings(order)
	}
	files := make([]string, 0, len(order))
	for _, f := range order {
		files = append(files, filepath.Join(d, f))
	}
	p := Project{Name: s.Name, Files: files, WorkingDir: d, Profiles: s.Profiles, SecretEnv: s.SecretEnv}
	if len(s.Env) > 0 {
		p.EnvFile = filepath.Join(d, ".env")
	}
	return p
}

// Materialise writes the project's compose files (+ rendered .env) to disk,
// asserts compose v2, and PULLS images. Pull is explicit here (its own error
// path) so Start's `up --pull never` fails loudly on a still-missing image
// rather than compose silently auto-pulling a fallback.
func (b *Backend) Materialise(ctx context.Context, s ProjectSpec) error {
	if s.Name == "" {
		return fmt.Errorf("compose backend: empty project name")
	}
	d := b.dir(s.Name)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return fmt.Errorf("compose backend: mkdir %s: %w", d, err)
	}
	for _, f := range s.FileOrder {
		if err := writeFile(filepath.Join(d, f), s.Files[f]); err != nil {
			return err
		}
	}
	// Catch files not listed in FileOrder too (defensive).
	for f, content := range s.Files {
		if !contains(s.FileOrder, f) {
			if err := writeFile(filepath.Join(d, f), content); err != nil {
				return err
			}
		}
	}
	if len(s.Env) > 0 {
		if err := writeFile(filepath.Join(d, ".env"), renderEnv(s.Env)); err != nil {
			return err
		}
	}
	if _, err := b.adapter.Version(ctx); err != nil {
		return err
	}
	return b.adapter.Pull(ctx, b.project(s))
}

// Start brings the project up detached (--pull never; images pulled in Materialise).
func (b *Backend) Start(ctx context.Context, s ProjectSpec) error {
	return b.adapter.Up(ctx, b.project(s))
}

// Stop stops the project's containers without removing them (lifecycle Stop).
func (b *Backend) Stop(ctx context.Context, s ProjectSpec) error {
	return b.adapter.Stop(ctx, b.project(s))
}

// Logs returns the tail of the project's aggregated compose logs.
func (b *Backend) Logs(ctx context.Context, s ProjectSpec, tail int) ([]byte, error) {
	return b.adapter.Logs(ctx, b.project(s), tail)
}

// Teardown runs `compose down`. removeVolumes drops anonymous volumes; tiered
// volumes are tierd-owned (bind-resolved) and NOT removed here. It also removes
// the on-disk project dir so a re-install starts clean.
func (b *Backend) Teardown(ctx context.Context, s ProjectSpec, removeVolumes bool) error {
	if err := b.adapter.Down(ctx, b.project(s), removeVolumes); err != nil {
		return err
	}
	return os.RemoveAll(b.dir(s.Name))
}

// Overall is the derived project-level state.
type Overall string

const (
	StateRunning  Overall = "running"  // every service up (and healthy where reported)
	StateDegraded Overall = "degraded" // some up, some not / some unhealthy
	StateStopped  Overall = "stopped"  // no containers present
	StateFailed   Overall = "failed"   // a container exited/dead
)

// Status is the compose-ps-derived state: the source of truth tierd caches.
type Status struct {
	Overall  Overall
	Services []PsEntry
}

// Status reads `compose ps` and derives a project-level Overall. This is the
// authoritative container state (not tierd's DB).
func (b *Backend) Status(ctx context.Context, s ProjectSpec) (Status, error) {
	entries, err := b.adapter.Ps(ctx, b.project(s))
	if err != nil {
		return Status{}, err
	}
	return Status{Overall: derive(entries), Services: entries}, nil
}

// derive folds per-container state into a project Overall.
func derive(entries []PsEntry) Overall {
	if len(entries) == 0 {
		return StateStopped
	}
	running, failed := 0, 0
	for _, e := range entries {
		switch e.State {
		case "running":
			// A running-but-unhealthy container is degraded, not fully up.
			if e.Health == "" || e.Health == "healthy" || e.Health == "starting" {
				running++
			}
		case "exited", "dead", "oom":
			failed++
		}
	}
	switch {
	case failed > 0:
		return StateFailed
	case running == len(entries):
		return StateRunning
	default:
		return StateDegraded
	}
}

func writeFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		return fmt.Errorf("compose backend: write %s: %w", path, err)
	}
	return nil
}

// renderEnv renders operator config to a deterministic .env (sorted keys).
// NOTE (roundtable): secrets should NOT ride here in plaintext — a follow-up
// slice routes secret-typed config through a restricted-perms path, not .env.
func renderEnv(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, env[k]...)
		b = append(b, '\n')
	}
	return string(b)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
