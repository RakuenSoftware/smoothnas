package plugin

import (
	"context"
	"reflect"
	"strings"
	"testing"

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
	// Full lifecycle routes to compose: Materialise=version+pull, Start=up,
	// Stop=stop, Demolish=down. None of it hit the manifest path.
	if want := []string{"version", "pull", "up", "stop", "down"}; !reflect.DeepEqual(r.subs, want) {
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

	rec, err := lc.Status(context.Background(), "demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rec.Plugin.State != StateRunning {
		t.Fatalf("compose Status state=%q, want %q (from compose ps, not DB)", rec.Plugin.State, StateRunning)
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
