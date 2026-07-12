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
	subs      []string
	psOut     []byte
	logsOut   []byte
	lastUpEnv []string
}

func (r *recRunner) Run(_ context.Context, env []string, args ...string) ([]byte, []byte, error) {
	for _, a := range args {
		switch a {
		case "version", "pull", "up", "stop", "down", "ps", "logs":
			r.subs = append(r.subs, a)
			if a == "up" {
				r.lastUpEnv = env
			}
		}
	}
	for _, a := range args {
		switch a {
		case "version":
			return []byte(`{"version":"v2.29.7"}`), nil, nil
		case "ps":
			return r.psOut, nil, nil
		case "logs":
			return r.logsOut, nil, nil
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
	tp := &fakeTierProvider{tiers: map[string]*db.TierInstance{}}
	mnt := tp.putTemp(t, "fast")

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
	if !strings.Contains(string(written), mnt) || strings.Contains(string(written), "x-smoothnas") {
		t.Fatalf("compose not rewritten to tier bind:\n%s", written)
	}
	// Materialise must create the bind-source dir, or lxc-start aborts on the
	// mount (ENOENT). It lives under <mount>/.plugins/<plugin>/compose/<vol>.
	if _, err := os.Stat(filepath.Join(mnt, ".plugins", "app", "compose", "data")); err != nil {
		t.Fatalf("tiered bind dir not created: %v", err)
	}
}

// TestLifecycle_ComposeTieredVolumePinGuardsRetier proves the pin prevents a
// compose edit from silently relocating tiered data: first Materialise pins the
// volume to its tier; a later Materialise whose compose points at a different
// tier is refused.
func TestLifecycle_ComposeTieredVolumePinGuardsRetier(t *testing.T) {
	store := openTestStore(t)
	fast := "name: app\nservices:\n  web:\n    image: nginx\n    volumes: [\"data:/var/data\"]\nvolumes:\n  data:\n    x-smoothnas: { tier: fast }\n"
	if _, err := NewInstaller(store).Install([]byte(fast)); err != nil {
		t.Fatalf("install: %v", err)
	}
	tp := &fakeTierProvider{tiers: map[string]*db.TierInstance{}}
	tp.putTemp(t, "fast")
	tp.putTemp(t, "slow")
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", &recRunner{}), t.TempDir()))
	lc.SetTierProvider(tp)

	if err := lc.Materialise(context.Background(), "app"); err != nil {
		t.Fatalf("first materialise: %v", err)
	}
	// Operator retiers data -> slow in the compose and reinstalls.
	if err := store.SetManifestYAML("app", strings.Replace(fast, "tier: fast", "tier: slow", 1)); err != nil {
		t.Fatal(err)
	}
	err := lc.Materialise(context.Background(), "app")
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("expected retier refusal, got %v", err)
	}
}

// TestLifecycle_ComposeServices returns the live per-service compose ps (Phase 4).
func TestLifecycle_ComposeServices(t *testing.T) {
	store := openTestStore(t)
	if _, err := NewInstaller(store).Install([]byte("name: app\nservices:\n  web:\n    image: nginx\n")); err != nil {
		t.Fatalf("install: %v", err)
	}
	r := &recRunner{psOut: []byte(`{"Name":"app-web-1","Service":"web","State":"running","Health":"healthy"}`)}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", r), t.TempDir()))

	st, err := lc.ComposeServices(context.Background(), "app")
	if err != nil {
		t.Fatalf("services: %v", err)
	}
	if string(st.Overall) != "running" || len(st.Services) != 1 || st.Services[0].Service != "web" {
		t.Fatalf("status=%+v", st)
	}
	// manifest (non-compose) plugin errors.
	if _, err := lc.ComposeServices(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("expected error for unknown/non-compose plugin")
	}
}

// TestLifecycle_ComposeLogs returns the compose logs tail for a compose plugin.
func TestLifecycle_ComposeLogs(t *testing.T) {
	store := openTestStore(t)
	if _, err := NewInstaller(store).Install([]byte("name: app\nservices:\n  web:\n    image: nginx\n")); err != nil {
		t.Fatalf("install: %v", err)
	}
	r := &recRunner{logsOut: []byte("web-1  | hello\n")}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", r), t.TempDir()))
	out, err := lc.ComposeLogs(context.Background(), "app", 50)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(string(out), "hello") {
		t.Fatalf("logs=%q", out)
	}
	found := false
	for _, s := range r.subs {
		if s == "logs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a compose logs subcommand, subs=%v", r.subs)
	}
}

// TestInstaller_ComposeTierOverride bakes the operator's install-time tier into
// the stored compose (portability fix): a plugin ships tier=default-pool but the
// operator remaps it to fast-pool without editing the file.
func TestInstaller_ComposeTierOverride(t *testing.T) {
	store := openTestStore(t)
	proj := "name: app\nservices:\n  web:\n    image: nginx\n    volumes: [\"data:/d\"]\nvolumes:\n  data:\n    x-smoothnas: { tier: default-pool }\n"
	rec, err := NewInstaller(store).InstallWithOptions([]byte(proj), InstallOptions{
		Tiers: TierAssignments{PerVolume: map[string]string{"data": "fast-pool"}},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	tv, _ := compose.TieredVolumes([]byte(rec.Plugin.ManifestYAML))
	if len(tv) != 1 || tv[0].Tier != "fast-pool" {
		t.Fatalf("stored compose tier not overridden: %+v", tv)
	}
}

// TestLifecycle_ComposeInstanceExpansion proves a scalable compose plugin
// materialises to N per-instance services, each with its own tier-bound _work.
func TestLifecycle_ComposeInstanceExpansion(t *testing.T) {
	store := openTestStore(t)
	proj := "name: gh-runner\nservices:\n  gh-runner:\n    image: ghcr.io/x/r:1\n    x-smoothnas:\n      instances: { count: 2, min: 1, max: 8 }\n    volumes: [\"work:/w\"]\nvolumes:\n  work:\n    x-smoothnas: { tier: fast, perInstance: true }\n"
	rec, err := NewInstaller(store).Install([]byte(proj))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rec.Plugin.InstanceCount != 2 || !rec.Plugin.InstanceConfigurable {
		t.Fatalf("expected count=2 configurable, got %d/%v", rec.Plugin.InstanceCount, rec.Plugin.InstanceConfigurable)
	}
	tp := &fakeTierProvider{tiers: map[string]*db.TierInstance{}}
	mnt := tp.putTemp(t, "fast")
	root := t.TempDir()
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", &recRunner{}), root))
	lc.SetTierProvider(tp)
	if err := lc.Materialise(context.Background(), "gh-runner"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, "gh-runner", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(written)
	for _, want := range []string{"gh-runner-1:", "gh-runner-2:", mnt, "gh-runner-1-work", "gh-runner-2-work"} {
		if !strings.Contains(s, want) {
			t.Fatalf("expanded compose missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "instances:") {
		t.Fatalf("x-smoothnas.instances should be gone from the materialised compose:\n%s", s)
	}
}

// TestLifecycle_ComposeSecretInjection proves an x-smoothnas.secrets value is
// stored out-of-band and injected into the `compose up` subprocess env (never
// written to the compose file).
func TestLifecycle_ComposeSecretInjection(t *testing.T) {
	store := openTestStore(t)
	proj := "name: app\nx-smoothnas:\n  secrets: [GH_RUNNER_TOKEN]\nservices:\n  r:\n    image: x\n    environment:\n      GH_RUNNER_TOKEN: \"${GH_RUNNER_TOKEN}\"\n"
	if _, err := NewInstaller(store).InstallWithOptions([]byte(proj), InstallOptions{
		Config: map[string]string{"GH_RUNNER_TOKEN": "s3cr3t"},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// stored in the secret store, and NOT in the compose file.
	secrets, _ := store.GetComposeSecrets("app")
	if secrets["GH_RUNNER_TOKEN"] != "s3cr3t" {
		t.Fatalf("secret not stored: %v", secrets)
	}
	rec, _ := store.Get("app")
	if strings.Contains(rec.Plugin.ManifestYAML, "s3cr3t") {
		t.Fatal("secret leaked into the stored compose file")
	}
	// Start injects it into the up subprocess env.
	r := &recRunner{}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", r), t.TempDir()))
	if err := lc.Start(context.Background(), "app"); err != nil {
		t.Fatalf("start: %v", err)
	}
	found := false
	for _, e := range r.lastUpEnv {
		if e == "GH_RUNNER_TOKEN=s3cr3t" {
			found = true
		}
	}
	if !found {
		t.Fatalf("secret not injected into up env: %v", r.lastUpEnv)
	}
}

// TestLifecycle_ComposeScale scales a gh-runner-style plugin 2->4->1 and enforces max.
func TestLifecycle_ComposeScale(t *testing.T) {
	store := openTestStore(t)
	proj := "name: gh-runner\nservices:\n  gh-runner:\n    image: x\n    x-smoothnas:\n      instances: { count: 2, min: 1, max: 8 }\n    volumes: [\"work:/w\"]\nvolumes:\n  work:\n    x-smoothnas: { tier: fast, perInstance: true }\n"
	if _, err := NewInstaller(store).Install([]byte(proj)); err != nil {
		t.Fatalf("install: %v", err)
	}
	tp := &fakeTierProvider{tiers: map[string]*db.TierInstance{}}
	tp.putTemp(t, "fast")
	root := t.TempDir()
	r := &recRunner{}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", r), root))
	lc.SetTierProvider(tp)

	res, err := lc.Scale(context.Background(), "gh-runner", 4)
	if err != nil {
		t.Fatalf("scale up: %v", err)
	}
	if res.From != 2 || res.To != 4 || len(res.Added) != 2 {
		t.Fatalf("scale result=%+v", res)
	}
	written, _ := os.ReadFile(filepath.Join(root, "gh-runner", "compose.yaml"))
	for _, want := range []string{"gh-runner-1:", "gh-runner-4:", "gh-runner-4-work"} {
		if !strings.Contains(string(written), want) {
			t.Fatalf("scaled compose missing %q:\n%s", want, written)
		}
	}
	// StartScaled ran a reconcile up (--remove-orphans).
	sawUp := false
	for _, sub := range r.subs {
		if sub == "up" {
			sawUp = true
		}
	}
	if !sawUp {
		t.Fatalf("expected a reconcile up, subs=%v", r.subs)
	}
	// above max is rejected.
	if _, err := lc.Scale(context.Background(), "gh-runner", 20); err == nil {
		t.Fatal("expected max violation")
	}
	// scale down reports removed.
	res2, err := lc.Scale(context.Background(), "gh-runner", 1)
	if err != nil {
		t.Fatalf("scale down: %v", err)
	}
	if res2.To != 1 || len(res2.Removed) != 3 {
		t.Fatalf("scale down result=%+v", res2)
	}
}

// TestGHRunner_ComposeEndToEnd drives the full gh-runner compose migration from
// the shipped fixture: install with a secret token, materialise -> N per-instance
// tier-bound services with the secret injected, then scale.
func TestGHRunner_ComposeEndToEnd(t *testing.T) {
	yamlBytes, err := os.ReadFile("testdata/gh-runner.compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t)
	rec, err := NewInstaller(store).InstallWithOptions(yamlBytes, InstallOptions{
		Config: map[string]string{"GH_RUNNER_TOKEN": "AABBCC"},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rec.Plugin.InstanceCount != 2 || !rec.Plugin.InstanceConfigurable {
		t.Fatalf("count/configurable = %d/%v", rec.Plugin.InstanceCount, rec.Plugin.InstanceConfigurable)
	}
	// secret stored out-of-band, not in the compose.
	if s, _ := store.GetComposeSecrets("gh-runner"); s["GH_RUNNER_TOKEN"] != "AABBCC" {
		t.Fatalf("secret not stored: %v", s)
	}
	if strings.Contains(rec.Plugin.ManifestYAML, "AABBCC") {
		t.Fatal("secret leaked into stored compose")
	}
	tp := &fakeTierProvider{tiers: map[string]*db.TierInstance{}}
	mnt := tp.putTemp(t, "runner-ssd")
	root := t.TempDir()
	r := &recRunner{}
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", r), root))
	lc.SetTierProvider(tp)

	if err := lc.Materialise(context.Background(), "gh-runner"); err != nil {
		t.Fatalf("materialise: %v", err)
	}
	if err := lc.Start(context.Background(), "gh-runner"); err != nil {
		t.Fatalf("start: %v", err)
	}
	written, _ := os.ReadFile(filepath.Join(root, "gh-runner", "compose.yaml"))
	for _, want := range []string{"gh-runner-1:", "gh-runner-2:", "gh-runner-1-work", mnt} {
		if !strings.Contains(string(written), want) {
			t.Fatalf("materialised compose missing %q:\n%s", want, written)
		}
	}
	// secret reached the up subprocess env.
	tokenInEnv := false
	for _, e := range r.lastUpEnv {
		if e == "GH_RUNNER_TOKEN=AABBCC" {
			tokenInEnv = true
		}
	}
	if !tokenInEnv {
		t.Fatal("token not injected into up env")
	}
	// scale to 5.
	res, err := lc.Scale(context.Background(), "gh-runner", 5)
	if err != nil || res.To != 5 {
		t.Fatalf("scale: %+v err=%v", res, err)
	}
}

// A compose plugin records each service's image as a plugin_services +
// plugin_container_refs row, so it surfaces the same Update button as a manifest
// plugin (the API's containerUpdateAvailable = hasMutableContainerRef, which
// reads these refs). Before this, InsertCompose wrote no refs and the button
// never appeared even though refresh-containers (compose pull + up) works.
func TestInstaller_ComposeRecordsServiceImagesForUpdates(t *testing.T) {
	store := openTestStore(t)
	const project = "name: app\n" +
		"services:\n" +
		"  web:\n" +
		"    image: ghcr.io/acme/web:latest\n" +
		"  db:\n" +
		"    image: pgvector/pgvector:pg17\n"
	if _, err := NewInstaller(store).Install([]byte(project)); err != nil {
		t.Fatalf("install: %v", err)
	}
	rec, err := store.Get("app")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(rec.ContainerRefs) != 2 {
		t.Fatalf("container refs = %d, want 2 (one per service)", len(rec.ContainerRefs))
	}
	got := map[string]string{}
	for _, r := range rec.ContainerRefs {
		if r.Name != "primary" {
			t.Fatalf("ref name = %q, want primary", r.Name)
		}
		got[r.Service] = r.ImageRef
		// A mutable tag (no @sha256) is what drives the Update button.
		if strings.Contains(r.ImageRef, "@sha256:") {
			t.Fatalf("ref %s image %q is digest-pinned; update button would not show", r.Service, r.ImageRef)
		}
	}
	if got["web"] != "ghcr.io/acme/web:latest" || got["db"] != "pgvector/pgvector:pg17" {
		t.Fatalf("recorded images = %+v, want web+db", got)
	}
	// The service rows exist too (container_refs FK requires them) but stay inert
	// for lifecycle — the plugin is still artifact_type=compose.
	if rec.Plugin.ArtifactType != ArtifactCompose {
		t.Fatalf("artifact_type = %q, want compose", rec.Plugin.ArtifactType)
	}
	if len(rec.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(rec.Services))
	}
}

// BackfillComposeImageRefs gives compose plugins installed before image-ref
// tracking an Update button on upgrade, without a reinstall — and is idempotent.
func TestLifecycle_BackfillComposeImageRefs(t *testing.T) {
	store := openTestStore(t)
	// Simulate a pre-fix compose install: a plugins row + stored project, but no
	// service/container-ref rows (the old InsertCompose path).
	const project = "name: legacy\nservices:\n  web:\n    image: nginx:latest\n"
	if err := store.InsertCompose("legacy", ""); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.SetManifestYAML("legacy", project); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if rec, _ := store.Get("legacy"); len(rec.ContainerRefs) != 0 {
		t.Fatalf("precondition: want 0 refs, got %d", len(rec.ContainerRefs))
	}

	lc := NewLifecycle(store, &fakeRuntime{})
	if err := lc.BackfillComposeImageRefs(context.Background()); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	rec, _ := store.Get("legacy")
	if len(rec.ContainerRefs) != 1 || rec.ContainerRefs[0].ImageRef != "nginx:latest" {
		t.Fatalf("after backfill: refs=%+v", rec.ContainerRefs)
	}
	// Idempotent: a second sweep leaves the single ref untouched (already tracked).
	if err := lc.BackfillComposeImageRefs(context.Background()); err != nil {
		t.Fatalf("backfill 2: %v", err)
	}
	if rec, _ := store.Get("legacy"); len(rec.ContainerRefs) != 1 {
		t.Fatalf("idempotency: refs=%d, want 1", len(rec.ContainerRefs))
	}
}
