package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// openTestStore mirrors db's own helper: temp dir, open, migrate,
// register a Cleanup. Tests get a fresh DB each time.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	dbStore, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dbStore.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { dbStore.Close() })
	return NewStore(dbStore)
}

func mustParse(t *testing.T, file string) *Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return m
}

// pathsFor builds the service→volume→instance host-path map for a
// manifest. Flat volumes get a real path under root; tier-bound volumes
// get the empty-path sentinel (resolved later by the tier provider).
func pathsFor(m *Manifest, root string) map[string]map[string]map[int]string {
	out := map[string]map[string]map[int]string{}
	count := m.EffectiveCount()
	for si := range m.Services {
		svc := &m.Services[si]
		seg := servicePathSegment(m.Metadata.Name, svc.Name)
		vols := map[string]map[int]string{}
		for _, vol := range svc.Volumes {
			entries := map[int]string{}
			n := 1
			if vol.PerInstance {
				n = count
			}
			for i := 1; i <= n; i++ {
				host := ""
				if vol.Mode == VolumeModeFlat {
					if vol.PerInstance {
						host = filepath.Join(root, m.Metadata.Name, seg, fmt.Sprintf("instance-%d", i), vol.Name)
					} else {
						host = filepath.Join(root, m.Metadata.Name, seg, vol.Name)
					}
				}
				entries[i] = host
			}
			vols[vol.Name] = entries
		}
		out[svc.Name] = vols
	}
	return out
}

// cloneServices deep-copies the per-service slices tests mutate so a
// `*m` shallow copy doesn't alias the original manifest's backing arrays.
func cloneServices(in []Service) []Service {
	out := make([]Service, len(in))
	copy(out, in)
	for i := range out {
		out[i].Ports = append([]Port(nil), in[i].Ports...)
		out[i].Config = append([]ConfigField(nil), in[i].Config...)
		out[i].Volumes = append([]Volume(nil), in[i].Volumes...)
	}
	return out
}

func TestStore_InsertGet_SingleInstance(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsFor(m, "/var/lib/smoothnas/plugins"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rec, err := s.Get(m.Metadata.Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Plugin.Name != "llama-cpp" {
		t.Errorf("name = %q", rec.Plugin.Name)
	}
	if rec.Plugin.State != StateInstalled {
		t.Errorf("state = %q want %q", rec.Plugin.State, StateInstalled)
	}
	if rec.Plugin.InstanceCount != 1 {
		t.Errorf("instance_count = %d want 1", rec.Plugin.InstanceCount)
	}
	if len(rec.Services) != 1 || rec.Services[0].Service != "llama-cpp" {
		t.Fatalf("services = %+v want one named llama-cpp", rec.Services)
	}
	if len(rec.Instances) != 1 {
		t.Fatalf("instances = %d want 1", len(rec.Instances))
	}
	if rec.Instances[0].State != StateInstalled {
		t.Errorf("instance[0].state = %q", rec.Instances[0].State)
	}
	if rec.Instances[0].Service != "llama-cpp" {
		t.Errorf("instance[0].service = %q", rec.Instances[0].Service)
	}
	if len(rec.Volumes) != 1 {
		t.Fatalf("volumes = %d want 1", len(rec.Volumes))
	}
	v := rec.Volumes[0]
	if v.Name != "models" || v.Mode != VolumeModeTierBound || v.Slot != "NVME" {
		t.Errorf("volume mismatch: %+v", v)
	}
	if v.TierPool != "<unresolved>" {
		t.Errorf("tier_pool = %q want <unresolved>", v.TierPool)
	}
	if got, want := len(v.Paths), 1; got != want {
		t.Errorf("volume paths = %d want %d", got, want)
	}
	if rec.Volumes[0].Paths[1] != "" {
		t.Errorf("tier-bound volume should have empty path, got %q", rec.Volumes[0].Paths[1])
	}
	if len(rec.Ports) != 1 || rec.Ports[0].ContainerPort != 8080 {
		t.Errorf("ports = %+v", rec.Ports)
	}
	if len(rec.Config) != 1 || rec.Config[0].Key != "MODEL_PATH" {
		t.Errorf("config = %+v", rec.Config)
	}
	if rec.Config[0].Value != "/models/default.gguf" {
		t.Errorf("config default value = %q", rec.Config[0].Value)
	}
}

func TestStore_Insert_NormalizesEmbeddedImageDigest(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	digest := "sha256:" + strings.Repeat("a", 64)
	m.Services[0].Artifact.Image = "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:0.2.0-vulkan@" + digest
	m.Services[0].Artifact.Digest = ""

	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsFor(m, "/var/lib/smoothnas/plugins"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rec, err := s.Get(m.Metadata.Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp@" + digest
	if rec.Plugin.ImageRef != want {
		t.Fatalf("image_ref = %q want %q", rec.Plugin.ImageRef, want)
	}
	if rec.Services[0].ImageRef != want {
		t.Fatalf("service image_ref = %q want %q", rec.Services[0].ImageRef, want)
	}
}

func TestStore_Insert_PersistsContainerRefs(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	m.Services[0].ContainerRefs = []ContainerRef{{
		Name:  "sidecar",
		Image: "ghcr.io/example/sidecar:latest",
	}}

	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsFor(m, "/var/lib/smoothnas/plugins"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rec, err := s.Get(m.Metadata.Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(rec.ContainerRefs) != 2 {
		t.Fatalf("container refs = %+v, want primary + sidecar", rec.ContainerRefs)
	}
	byName := map[string]ContainerRefRow{}
	for _, ref := range rec.ContainerRefs {
		byName[ref.Name] = ref
	}
	if byName["primary"].Service != "llama-cpp" || byName["sidecar"].Service != "llama-cpp" {
		t.Fatalf("service names not recorded: %+v", rec.ContainerRefs)
	}
	if byName["primary"].ImageRef == "" {
		t.Fatalf("missing primary ref: %+v", rec.ContainerRefs)
	}
	if byName["sidecar"].ImageRef != "ghcr.io/example/sidecar:latest" {
		t.Fatalf("sidecar ref = %+v", byName["sidecar"])
	}
}

func TestStore_UpdateManifestPreservesOperatorConfig(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsFor(m, "/var/lib/smoothnas/plugins"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.ReplaceConfig(m.Metadata.Name, map[string]string{"MODEL_PATH": "/models/custom.gguf"}); err != nil {
		t.Fatalf("replace config: %v", err)
	}

	updated := *m
	updated.Services = cloneServices(m.Services)
	updated.Metadata.Version = "9.9.9"
	updated.Services[0].Artifact.Image = "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:9.9.9"
	updated.Services[0].Ports = []Port{{
		Name: "api", Port: 9090, Protocol: "tcp", Expose: true,
	}}
	updated.Services[0].Config = append(updated.Services[0].Config, ConfigField{
		Key: "EXTRA_FLAG", Type: "string", Default: "on",
	})

	if err := s.UpdateManifest(m.Metadata.Name, &updated, "updated manifest yaml"); err != nil {
		t.Fatalf("update manifest: %v", err)
	}

	rec, err := s.Get(m.Metadata.Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Plugin.Version != "9.9.9" {
		t.Fatalf("version = %q", rec.Plugin.Version)
	}
	if rec.Plugin.ManifestYAML != "updated manifest yaml" {
		t.Fatalf("manifest yaml = %q", rec.Plugin.ManifestYAML)
	}
	wantImage := "ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp@sha256:abababababababababababababababababababababababababababababababab"
	if rec.Plugin.ImageRef != wantImage {
		t.Fatalf("image ref = %q", rec.Plugin.ImageRef)
	}
	if len(rec.Ports) != 1 || rec.Ports[0].ContainerPort != 9090 {
		t.Fatalf("ports = %+v", rec.Ports)
	}
	gotConfig := map[string]string{}
	for _, row := range rec.Config {
		gotConfig[row.Key] = row.Value
	}
	if gotConfig["MODEL_PATH"] != "/models/custom.gguf" {
		t.Fatalf("MODEL_PATH = %q", gotConfig["MODEL_PATH"])
	}
	if gotConfig["EXTRA_FLAG"] != "on" {
		t.Fatalf("EXTRA_FLAG = %q", gotConfig["EXTRA_FLAG"])
	}
}

func TestStore_UpdateManifestRejectsVolumeShapeChange(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsFor(m, "/var/lib/smoothnas/plugins"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	updated := *m
	updated.Services = cloneServices(m.Services)
	updated.Metadata.Version = "9.9.9"
	updated.Services[0].Volumes = append(updated.Services[0].Volumes, Volume{
		Name: "cache", Mode: VolumeModeFlat, Bind: "/cache",
	})

	err := s.UpdateManifest(m.Metadata.Name, &updated, "updated manifest yaml")
	if !errors.Is(err, ErrPluginUpdateRequiresReinstall) {
		t.Fatalf("err = %v, want ErrPluginUpdateRequiresReinstall", err)
	}
}

func TestStore_InsertGet_MultiInstance_PerInstanceVolume(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "gh-runner.yaml") // count: 2, perInstance workspace
	if err := s.Insert(InsertParams{Manifest: m, Paths: pathsFor(m, "/tmp")}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rec, err := s.Get("gh-runner")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Plugin.InstanceCount != 2 || !rec.Plugin.InstanceConfigurable {
		t.Errorf("instance fields wrong: count=%d configurable=%v",
			rec.Plugin.InstanceCount, rec.Plugin.InstanceConfigurable)
	}
	if len(rec.Instances) != 2 {
		t.Fatalf("instances = %d want 2", len(rec.Instances))
	}
	for i, inst := range rec.Instances {
		if inst.Instance != i+1 {
			t.Errorf("instance[%d].Instance = %d", i, inst.Instance)
		}
		if inst.State != StateInstalled {
			t.Errorf("instance[%d].State = %q", i, inst.State)
		}
	}
	if len(rec.Volumes) != 1 {
		t.Fatalf("volumes = %d want 1", len(rec.Volumes))
	}
	v := rec.Volumes[0]
	if !v.PerInstance {
		t.Error("workspace volume should be perInstance")
	}
	if got, want := len(v.Paths), 2; got != want {
		t.Errorf("workspace paths = %d want %d", got, want)
	}
	if len(rec.Config) != 3 {
		t.Errorf("config rows = %d want 3", len(rec.Config))
	}
}

func TestStore_Insert_DuplicateNameReturnsErrPluginExists(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	paths := pathsFor(m, "/tmp")
	if err := s.Insert(InsertParams{Manifest: m, Paths: paths}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := s.Insert(InsertParams{Manifest: m, Paths: paths})
	if !errors.Is(err, ErrPluginExists) {
		t.Errorf("second insert err = %v, want ErrPluginExists", err)
	}
}

func TestStore_Get_MissingReturnsErrPluginNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Get("nope")
	if !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("get missing err = %v, want ErrPluginNotFound", err)
	}
}

func TestStore_Delete_CascadesAndIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "gh-runner.yaml")
	if err := s.Insert(InsertParams{Manifest: m, Paths: pathsFor(m, "/tmp")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.Delete("gh-runner"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Cascade: every child table should be empty for this plugin.
	for _, tbl := range []string{
		"plugin_services", "plugin_instances", "plugin_volumes",
		"plugin_volume_paths", "plugin_ports", "plugin_config",
	} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+tbl+` WHERE plugin_name = ?`, "gh-runner").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows after delete", tbl, n)
		}
	}

	if err := s.Delete("gh-runner"); !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("second delete err = %v, want ErrPluginNotFound", err)
	}
}

func TestStore_List_OrdersByName(t *testing.T) {
	s := openTestStore(t)
	for _, file := range []string{"llama.yaml", "gh-runner.yaml", "ubuntu-python.yaml"} {
		m := mustParse(t, file)
		if err := s.Insert(InsertParams{Manifest: m, Paths: pathsFor(m, "/tmp")}); err != nil {
			t.Fatalf("insert %s: %v", file, err)
		}
	}
	rows, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"gh-runner", "llama-cpp", "ubuntu-python"}
	if len(rows) != len(want) {
		t.Fatalf("len = %d want %d", len(rows), len(want))
	}
	for i, n := range want {
		if rows[i].Name != n {
			t.Errorf("rows[%d].Name = %q want %q", i, rows[i].Name, n)
		}
	}
}

func TestStore_SetInstanceState_RecomputesAggregate(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "gh-runner.yaml")
	if err := s.Insert(InsertParams{Manifest: m, Paths: pathsFor(m, "/tmp")}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Both instances → running. Aggregate should be "running".
	for inst := 1; inst <= 2; inst++ {
		if err := s.SetInstanceState("gh-runner", "gh-runner", inst, StateRunning, ""); err != nil {
			t.Fatalf("set instance %d running: %v", inst, err)
		}
	}
	rec, err := s.Get("gh-runner")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Plugin.State != StateRunning {
		t.Errorf("aggregate = %q want running", rec.Plugin.State)
	}

	// Fail one → degraded.
	if err := s.SetInstanceState("gh-runner", "gh-runner", 2, StateFailed, "boom"); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	rec, _ = s.Get("gh-runner")
	if rec.Plugin.State != StateDegraded {
		t.Errorf("aggregate = %q want degraded", rec.Plugin.State)
	}

	// Mark instance 1 as starting → transitional wins.
	if err := s.SetInstanceState("gh-runner", "gh-runner", 1, StateStarting, ""); err != nil {
		t.Fatalf("set starting: %v", err)
	}
	rec, _ = s.Get("gh-runner")
	if rec.Plugin.State != StateStarting {
		t.Errorf("aggregate = %q want starting (transitional wins)", rec.Plugin.State)
	}
}

func TestAggregateState_AllCases(t *testing.T) {
	cases := []struct {
		name   string
		counts map[string]int
		total  int
		want   string
	}{
		{"all running", map[string]int{StateRunning: 3}, 3, StateRunning},
		{"all stopped", map[string]int{StateStopped: 2}, 2, StateStopped},
		{"all failed", map[string]int{StateFailed: 4}, 4, StateFailed},
		{"all installed", map[string]int{StateInstalled: 1}, 1, StateInstalled},
		{"running and failed", map[string]int{StateRunning: 1, StateFailed: 1}, 2, StateDegraded},
		{"running and stopped", map[string]int{StateRunning: 1, StateStopped: 1}, 2, StateDegraded},
		{"any pulling wins", map[string]int{StatePulling: 1, StateRunning: 2}, 3, StatePulling},
		{"any creating wins", map[string]int{StateCreating: 1, StateFailed: 2}, 3, StateCreating},
		{"any starting wins", map[string]int{StateStarting: 1, StateRunning: 1}, 2, StateStarting},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateState(tc.counts, tc.total); got != tc.want {
				t.Errorf("aggregateState(%v) = %q want %q", tc.counts, got, tc.want)
			}
		})
	}
}
