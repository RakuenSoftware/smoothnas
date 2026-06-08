package plugin

import "testing"

// Discovery resolves a sibling to its stable container name (not a bridge
// IP). The runtime (LXC2Docker) owns name→IP resolution via /etc/hosts, so
// a dependent's injected env (e.g. AIMEE_DB2_URL) keeps working across a
// backend's IP drift without recreating the dependent.
func TestCurrentDiscoveryUsesStableNames(t *testing.T) {
	rec := &PluginRecord{
		Plugin:   PluginRow{Name: "aimee-kb", InstanceCount: 1},
		Services: []ServiceRow{{Service: "postgres"}, {Service: "kb"}},
		// No BridgeIPs recorded — discovery must still yield names.
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
	got := renderDiscovery("postgresql://u:p@{{service.postgres.host}}:5432/db", disc)
	if want := "postgresql://u:p@aimee-kb-postgres:5432/db"; got != want {
		t.Errorf("rendered = %q, want %q", got, want)
	}
}
