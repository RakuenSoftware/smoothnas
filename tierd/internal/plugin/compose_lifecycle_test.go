package plugin

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
)

// recRunner records the compose subcommands it is asked to run and serves a
// v2 version so the Backend's version assertion passes. It implements
// compose.Runner so the whole install->route->compose path can be exercised
// without a live engine.
type recRunner struct {
	subs  []string
	psOut []byte
}

func (r *recRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
	for _, a := range args {
		switch a {
		case "version", "pull", "up", "stop", "down", "ps":
			r.subs = append(r.subs, a)
		}
	}
	for _, a := range args {
		switch a {
		case "version":
			return []byte(`{"version":"v2.29.7"}`), nil, nil
		case "ps":
			return r.psOut, nil, nil
		}
	}
	return nil, nil, nil
}

// TestLifecycle_RoutesComposePluginToBackend proves the plugins-11 wiring: a
// compose-format plugin installs as artifact_type=compose and its lifecycle
// drives real `docker compose` (version+pull on Materialise, up on Start) —
// i.e. it routes to the compose Backend, not the manifest BuildCreatePayload
// path (which would call the runtime client, never compose).
func TestLifecycle_RoutesComposePluginToBackend(t *testing.T) {
	store := openTestStore(t)
	inst := NewInstaller(store)

	const project = "name: demo\nservices:\n  web:\n    image: nginx\n"
	rec, err := inst.Install([]byte(project))
	if err != nil {
		t.Fatalf("install compose plugin: %v", err)
	}
	if rec.Plugin.Name != "demo" {
		t.Fatalf("project name=%q, want demo (from compose name:)", rec.Plugin.Name)
	}
	if rec.Plugin.ArtifactType != ArtifactCompose {
		t.Fatalf("artifact_type=%q, want %q", rec.Plugin.ArtifactType, ArtifactCompose)
	}
	if rec.Plugin.ManifestYAML != project {
		t.Fatalf("stored project mismatch:\n%q", rec.Plugin.ManifestYAML)
	}

	r := &recRunner{}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", r), t.TempDir()))

	if err := lc.Materialise(context.Background(), "demo"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if err := lc.Start(context.Background(), "demo"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := lc.Stop(context.Background(), "demo"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := lc.Demolish(context.Background(), "demo"); err != nil {
		t.Fatalf("demolish: %v", err)
	}
	// Full lifecycle routes to compose: Materialise=version+pull, Start=up then a
	// state-sync ps, Stop=stop then ps, Demolish=down. None of it hit the manifest path.
	if want := []string{"version", "pull", "up", "ps", "stop", "ps", "down"}; !reflect.DeepEqual(r.subs, want) {
		t.Fatalf("compose subcommands=%v, want %v (routed to backend?)", r.subs, want)
	}
}

// TestLifecycle_ComposeStatusReflectsPs proves Status uses compose ps as the
// source of truth for a compose plugin (state = running from a live ps), not
// the stale DB state (installed).
func TestLifecycle_ComposeStatusReflectsPs(t *testing.T) {
	store := openTestStore(t)
	if _, err := NewInstaller(store).Install([]byte("name: demo\nservices:\n  web:\n    image: nginx\n")); err != nil {
		t.Fatalf("install: %v", err)
	}
	r := &recRunner{psOut: []byte(`{"Name":"demo-web-1","Service":"web","State":"running","Health":"healthy"}`)}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", r), t.TempDir()))

	// Start caches the REAL rollup from compose ps; Status then reads that cache.
	if err := lc.Materialise(context.Background(), "demo"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if err := lc.Start(context.Background(), "demo"); err != nil {
		t.Fatalf("start: %v", err)
	}
	rec, err := lc.Status(context.Background(), "demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rec.Plugin.State != StateRunning {
		t.Fatalf("state=%q, want %q (cached from compose ps at Start)", rec.Plugin.State, StateRunning)
	}
}

// TestLifecycle_ComposeHostPortConflict proves the cross-project host-port guard
// rejects a second compose plugin claiming an already-published host port.
func TestLifecycle_ComposeHostPortConflict(t *testing.T) {
	store := openTestStore(t)
	inst := NewInstaller(store)
	a := "name: a\nservices:\n  w:\n    image: nginx\n    ports:\n      - \"8080:80\"\n"
	b := "name: b\nservices:\n  w:\n    image: nginx\n    ports:\n      - \"8080:80\"\n"
	if _, err := inst.Install([]byte(a)); err != nil {
		t.Fatalf("install a: %v", err)
	}
	if _, err := inst.Install([]byte(b)); err != nil {
		t.Fatalf("install b: %v", err)
	}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", &recRunner{}), t.TempDir()))
	err := lc.Materialise(context.Background(), "b")
	if err == nil || !strings.Contains(err.Error(), "8080/tcp") {
		t.Fatalf("expected host-port conflict on 8080/tcp, got %v", err)
	}
}

// TestLifecycle_ReconcileComposeStates syncs a compose plugin's cached state from
// compose ps (the periodic sweep for out-of-band drift). A manifest plugin is
// untouched by the sweep.
func TestLifecycle_ReconcileComposeStates(t *testing.T) {
	store := openTestStore(t)
	if _, err := NewInstaller(store).Install([]byte("name: demo\nservices:\n  web:\n    image: nginx\n")); err != nil {
		t.Fatalf("install: %v", err)
	}
	r := &recRunner{psOut: []byte(`{"Name":"demo-web-1","Service":"web","State":"running","Health":"healthy"}`)}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", r), t.TempDir()))
	if err := lc.ReconcileComposeStates(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.Get("demo")
	if rec.Plugin.State != StateRunning {
		t.Fatalf("state=%q want running (synced by reconcile sweep)", rec.Plugin.State)
	}
}

// TestLifecycle_ComposeTieredVolumeRewrite proves Materialise resolves an
// x-smoothnas tiered volume and writes a compose file that binds the resolved
// tier host path (mechanism B).
func TestLifecycle_ComposeTieredVolumeRewrite(t *testing.T) {
	store := openTestStore(t)
	proj := "name: app\nservices:\n  web:\n    image: nginx\n    volumes: [\"data:/var/data\"]\nvolumes:\n  data:\n    x-smoothnas: { tier: fast }\n"
	if _, err := NewInstaller(store).Install([]byte(proj)); err != nil {
		t.Fatalf("install: %v", err)
	}
	tp := &fakeTierProvider{tiers: map[string]*db.TierInstance{}, slots: map[string][]db.TierSlot{}}
	tp.put("fast", "/mnt/fast", "healthy")

	root := t.TempDir()
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", &recRunner{}), root))
	lc.SetTierProvider(tp)

	if err := lc.Materialise(context.Background(), "app"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, "app", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "/mnt/fast") || strings.Contains(string(written), "x-smoothnas") {
		t.Fatalf("compose not rewritten to tier bind:\n%s", written)
	}
}
