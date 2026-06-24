package plugin

import (
	"strings"
	"testing"
)

// A hostExpose port is published on the host, so two plugins claiming the same
// host port silently shadow each other. checkHostPortConflicts must reject that
// before materialise rather than ship a "running" but unreachable plugin.
func TestLifecycle_checkHostPortConflicts(t *testing.T) {
	store := openTestStore(t)
	lc := NewLifecycle(store, &fakeRuntime{})

	// Existing plugin: wolf host-exposes 47984/tcp, 47999/udp, 48010/tcp, ...
	wolf := mustParse(t, "wolf.yaml")
	if err := store.Insert(InsertParams{Manifest: wolf, Paths: pathsFor(wolf, "/tmp")}); err != nil {
		t.Fatalf("insert wolf: %v", err)
	}

	rec := func(ports ...PortRow) *PluginRecord {
		return &PluginRecord{Plugin: PluginRow{Name: "aimee-llm"}, Ports: ports}
	}
	hp := func(port int, proto string) PortRow {
		return PortRow{PluginName: "aimee-llm", Service: "llm", Name: "gateway",
			ContainerPort: port, Protocol: proto, HostExpose: true}
	}

	t.Run("same tcp host port is rejected and names the holder", func(t *testing.T) {
		err := lc.checkHostPortConflicts("aimee-llm", rec(hp(47984, "tcp")))
		if err == nil || !strings.Contains(err.Error(), "wolf") {
			t.Fatalf("want conflict naming wolf, got %v", err)
		}
	})

	t.Run("a free host port is allowed", func(t *testing.T) {
		if err := lc.checkHostPortConflicts("aimee-llm", rec(hp(8742, "tcp"))); err != nil {
			t.Fatalf("want nil for free port, got %v", err)
		}
	})

	t.Run("same number different protocol does not conflict", func(t *testing.T) {
		// wolf publishes 47999/udp; 47999/tcp is a distinct host binding.
		if err := lc.checkHostPortConflicts("aimee-llm", rec(hp(47999, "tcp"))); err != nil {
			t.Fatalf("udp vs tcp must not conflict, got %v", err)
		}
	})

	t.Run("an internal-only (non-hostExpose) port does not conflict", func(t *testing.T) {
		p := hp(47984, "tcp")
		p.HostExpose = false
		if err := lc.checkHostPortConflicts("aimee-llm", rec(p)); err != nil {
			t.Fatalf("internal-only port must not conflict, got %v", err)
		}
	})

	t.Run("a plugin does not conflict with its own stored ports", func(t *testing.T) {
		wrec, err := store.Get("wolf")
		if err != nil {
			t.Fatalf("get wolf: %v", err)
		}
		if err := lc.checkHostPortConflicts("wolf", wrec); err != nil {
			t.Fatalf("self-check must pass, got %v", err)
		}
	})
}
