package plugin

import (
	"context"
	"reflect"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
)

// recRunner records the compose subcommands it is asked to run and serves a
// v2 version so the Backend's version assertion passes. It implements
// compose.Runner so the whole install->route->compose path can be exercised
// without a live engine.
type recRunner struct{ subs []string }

func (r *recRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
	for _, a := range args {
		switch a {
		case "version", "pull", "up", "stop", "down", "ps":
			r.subs = append(r.subs, a)
		}
	}
	for _, a := range args {
		if a == "version" {
			return []byte(`{"version":"v2.29.7"}`), nil, nil
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
