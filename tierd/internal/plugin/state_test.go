package plugin

import (
	"errors"
	"os"
	"path/filepath"
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

// pathsForSingleInstance is a small helper to construct the Paths
// map for a single-instance plugin where every volume is shared.
func pathsForSingleInstance(m *Manifest, root string) map[string]map[int]string {
	out := map[string]map[int]string{}
	for _, vol := range m.Volumes {
		host := ""
		if vol.Mode == VolumeModeFlat {
			host = filepath.Join(root, m.Metadata.Name, vol.Name)
		}
		out[vol.Name] = map[int]string{1: host}
	}
	return out
}

func TestStore_InsertGet_SingleInstance(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	if err := s.Insert(InsertParams{
		Manifest: m,
		Paths:    pathsForSingleInstance(m, "/var/lib/smoothnas/plugins"),
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
	if len(rec.Instances) != 1 {
		t.Fatalf("instances = %d want 1", len(rec.Instances))
	}
	if rec.Instances[0].State != StateInstalled {
		t.Errorf("instance[0].state = %q", rec.Instances[0].State)
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
		t.Errorf("tier-bound volume should have empty path in phase 1, got %q", rec.Volumes[0].Paths[1])
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

func TestStore_InsertGet_MultiInstance_PerInstanceVolume(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "gh-runner.yaml") // count: 2, perInstance workspace
	paths := map[string]map[int]string{
		"workspace": {
			1: "", // tier-bound, unresolved in phase 1
			2: "",
		},
	}
	if err := s.Insert(InsertParams{Manifest: m, Paths: paths}); err != nil {
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
	// Three config rows in the manifest, all with default "" except labels.
	if len(rec.Config) != 3 {
		t.Errorf("config rows = %d want 3", len(rec.Config))
	}
}

func TestStore_Insert_DuplicateNameReturnsErrPluginExists(t *testing.T) {
	s := openTestStore(t)
	m := mustParse(t, "llama.yaml")
	paths := pathsForSingleInstance(m, "/tmp")
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
	paths := map[string]map[int]string{
		"workspace": {1: "", 2: ""},
	}
	if err := s.Insert(InsertParams{Manifest: m, Paths: paths}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.Delete("gh-runner"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Cascade: every child table should be empty for this plugin.
	for _, tbl := range []string{
		"plugin_instances", "plugin_volumes", "plugin_volume_paths",
		"plugin_ports", "plugin_config",
	} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+tbl+` WHERE plugin_name = ?`, "gh-runner").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows after delete", tbl, n)
		}
	}

	// Second delete returns ErrPluginNotFound.
	if err := s.Delete("gh-runner"); !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("second delete err = %v, want ErrPluginNotFound", err)
	}
}

func TestStore_List_OrdersByName(t *testing.T) {
	s := openTestStore(t)
	for _, file := range []string{"llama.yaml", "gh-runner.yaml", "ubuntu-python.yaml"} {
		m := mustParse(t, file)
		paths := map[string]map[int]string{}
		for _, v := range m.Volumes {
			entries := map[int]string{}
			count := m.EffectiveCount()
			if v.PerInstance {
				for i := 1; i <= count; i++ {
					entries[i] = ""
				}
			} else {
				entries[1] = ""
			}
			paths[v.Name] = entries
		}
		if err := s.Insert(InsertParams{Manifest: m, Paths: paths}); err != nil {
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
	paths := map[string]map[int]string{"workspace": {1: "", 2: ""}}
	if err := s.Insert(InsertParams{Manifest: m, Paths: paths}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Both instances → running. Aggregate should be "running".
	for inst := 1; inst <= 2; inst++ {
		if err := s.SetInstanceState("gh-runner", inst, StateRunning, ""); err != nil {
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
	if err := s.SetInstanceState("gh-runner", 2, StateFailed, "boom"); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	rec, _ = s.Get("gh-runner")
	if rec.Plugin.State != StateDegraded {
		t.Errorf("aggregate = %q want degraded", rec.Plugin.State)
	}

	// Mark instance 1 as starting → transitional wins.
	if err := s.SetInstanceState("gh-runner", 1, StateStarting, ""); err != nil {
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
