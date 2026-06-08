package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

func TestRenderEtcHosts(t *testing.T) {
	got := renderEtcHosts([]hostEntry{
		{name: "aimee-kb-postgres", ip: "10.100.0.14"},
		{name: "aimee-kb-kb", ip: "10.100.0.15"},
		{name: "no-ip", ip: ""},          // dropped
		{name: "", ip: "10.0.0.9"},       // dropped
		{name: "aimee-kb-kb", ip: "9.9"}, // duplicate name dropped (first wins)
	})

	// localhost scaffolding is always present.
	for _, want := range []string{"127.0.0.1\tlocalhost", "::1\tlocalhost ip6-localhost ip6-loopback"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing localhost line %q in:\n%s", want, got)
		}
	}
	// Records sorted by name, first IP for a duplicate kept, empties dropped.
	wantOrder := "10.100.0.15\taimee-kb-kb\n10.100.0.14\taimee-kb-postgres\n"
	if !strings.Contains(got, wantOrder) {
		t.Errorf("records not in expected sorted form.\nwant substring:\n%s\ngot:\n%s", wantOrder, got)
	}
	if strings.Contains(got, "no-ip") || strings.Contains(got, "9.9") {
		t.Errorf("dropped entries leaked into output:\n%s", got)
	}
}

func TestContainerHostsPath(t *testing.T) {
	got := containerHostsPath(runtime.ContainerInspect{HostnamePath: "/srv/lxc/abc/rootfs/etc/hostname"})
	if want := "/srv/lxc/abc/rootfs/etc/hosts"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got := containerHostsPath(runtime.ContainerInspect{}); got != "" {
		t.Errorf("empty HostnamePath should yield empty path, got %q", got)
	}
}

func TestWriteHostsFileIdempotentAndAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	owner := filepath.Join(dir, "hostname")
	if err := os.WriteFile(owner, []byte("kb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeHostsFile(path, owner, "first\n"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "first\n" {
		t.Fatalf("content = %q", b)
	}

	// Re-writing identical content is a no-op that must not error and
	// must not leave temp files behind.
	if err := writeHostsFile(path, owner, "first\n"); err != nil {
		t.Fatalf("idempotent write: %v", err)
	}
	if err := writeHostsFile(path, owner, "second\n"); err != nil {
		t.Fatalf("changed write: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "second\n" {
		t.Fatalf("content after change = %q", b)
	}

	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".hosts-") {
			t.Errorf("leftover temp file %q — write not atomic/cleaned", e.Name())
		}
	}
}

// fakeHostsRuntime is a minimal hostsRuntime for syncManagedHosts.
type fakeHostsRuntime struct {
	list      []runtime.ContainerSummary
	inspect   map[string]runtime.ContainerInspect
	inspected int
}

func (f *fakeHostsRuntime) ListManagedContainers(context.Context) ([]runtime.ContainerSummary, error) {
	return f.list, nil
}

func (f *fakeHostsRuntime) InspectContainer(_ context.Context, id string) (runtime.ContainerInspect, error) {
	f.inspected++
	return f.inspect[id], nil
}

// newContainer wires up a managed container backed by a real temp
// rootfs so syncManagedHosts can write its /etc/hosts.
func newContainer(t *testing.T, id, name, plugin, ip string) (runtime.ContainerSummary, runtime.ContainerInspect) {
	t.Helper()
	etc := filepath.Join(t.TempDir(), "rootfs", "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	hostname := filepath.Join(etc, "hostname")
	if err := os.WriteFile(hostname, []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{runtime.PluginNameLabel: plugin}
	sum := runtime.ContainerSummary{ID: id, Names: []string{"/" + name}, Labels: labels}
	insp := runtime.ContainerInspect{
		ID:           id,
		Name:         "/" + name,
		HostnamePath: hostname,
		Config:       runtime.ContainerConfig{Labels: labels},
		NetworkSettings: runtime.ContainerNetworkSettings{
			Networks: map[string]runtime.ContainerNetwork{
				runtime.PluginBridgeName: {IPAddress: ip},
			},
		},
	}
	return sum, insp
}

func TestSyncManagedHostsGroupsByPlugin(t *testing.T) {
	kbSum, kbInsp := newContainer(t, "id-kb", "aimee-kb-kb", "aimee-kb", "10.100.0.15")
	pgSum, pgInsp := newContainer(t, "id-pg", "aimee-kb-postgres", "aimee-kb", "10.100.0.14")
	wolfSum, wolfInsp := newContainer(t, "id-wolf", "wolf", "wolf", "10.100.0.17")

	rt := &fakeHostsRuntime{
		list: []runtime.ContainerSummary{kbSum, pgSum, wolfSum},
		inspect: map[string]runtime.ContainerInspect{
			"id-kb": kbInsp, "id-pg": pgInsp, "id-wolf": wolfInsp,
		},
	}

	if err := syncManagedHosts(context.Background(), rt); err != nil {
		t.Fatalf("syncManagedHosts: %v", err)
	}

	read := func(insp runtime.ContainerInspect) string {
		b, err := os.ReadFile(containerHostsPath(insp))
		if err != nil {
			t.Fatalf("read hosts: %v", err)
		}
		return string(b)
	}

	// The kb resolves its same-plugin siblings (itself + postgres) but
	// NOT the unrelated wolf plugin.
	kbHosts := read(kbInsp)
	for _, want := range []string{"10.100.0.15\taimee-kb-kb", "10.100.0.14\taimee-kb-postgres"} {
		if !strings.Contains(kbHosts, want) {
			t.Errorf("kb /etc/hosts missing %q:\n%s", want, kbHosts)
		}
	}
	if strings.Contains(kbHosts, "wolf") {
		t.Errorf("kb /etc/hosts leaked cross-plugin entry:\n%s", kbHosts)
	}

	wolfHosts := read(wolfInsp)
	if !strings.Contains(wolfHosts, "10.100.0.17\twolf") {
		t.Errorf("wolf /etc/hosts missing self:\n%s", wolfHosts)
	}
	if strings.Contains(wolfHosts, "aimee-kb") {
		t.Errorf("wolf /etc/hosts leaked cross-plugin entry:\n%s", wolfHosts)
	}
}

func TestPluginHostEntriesSkipsMissingIPs(t *testing.T) {
	rec := &PluginRecord{
		Plugin:   PluginRow{Name: "aimee-kb", InstanceCount: 1},
		Services: []ServiceRow{{Service: "kb"}, {Service: "postgres"}, {Service: "embedder"}},
		Instances: []InstanceRow{
			{Service: "kb", Instance: 1, BridgeIP: "10.0.0.2"},
			{Service: "postgres", Instance: 1, BridgeIP: "10.0.0.3"},
			{Service: "embedder", Instance: 1, BridgeIP: ""}, // not up yet → skipped
		},
	}
	got := renderEtcHosts(pluginHostEntries(rec, 1))
	if !strings.Contains(got, "10.0.0.2\taimee-kb-kb") || !strings.Contains(got, "10.0.0.3\taimee-kb-postgres") {
		t.Errorf("expected kb+postgres records:\n%s", got)
	}
	if strings.Contains(got, "embedder") {
		t.Errorf("embedder with no IP should be skipped:\n%s", got)
	}
}

func TestCurrentDiscoveryUsesStableNames(t *testing.T) {
	rec := &PluginRecord{
		Plugin:   PluginRow{Name: "aimee-kb", InstanceCount: 1},
		Services: []ServiceRow{{Service: "postgres"}, {Service: "kb"}},
		// No BridgeIPs recorded at all — discovery must still yield names.
	}
	svcMap := map[string]*Service{
		"postgres": {Name: "postgres", Ports: []Port{{Name: "pg", Port: 5432}}},
		"kb":       {Name: "kb"},
	}
	disc := currentDiscovery(rec, svcMap)
	if got := disc["postgres"].Host; got != "aimee-kb-postgres" {
		t.Errorf("postgres discovery host = %q, want stable name aimee-kb-postgres", got)
	}
	if got := disc["postgres"].Ports["pg"]; got != 5432 {
		t.Errorf("postgres port = %d, want 5432", got)
	}
	// And the rendered token must be the name, so injected env survives drift.
	got := renderDiscovery("postgresql://u:p@{{service.postgres.host}}:5432/db", disc)
	if want := "postgresql://u:p@aimee-kb-postgres:5432/db"; got != want {
		t.Errorf("rendered = %q, want %q", got, want)
	}
}
