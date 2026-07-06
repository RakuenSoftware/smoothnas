package plugin

import (
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
)

func TestResolveComposeTierVolumes(t *testing.T) {
	tp := &fakeTierProvider{tiers: map[string]*db.TierInstance{}}
	tp.put("fast", "/mnt/fast", "healthy")

	binds, err := ResolveComposeTierVolumes(tp, "app", []compose.TieredVolume{{Name: "data", Tier: "fast"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(binds["data"], "/mnt/fast") || !strings.Contains(binds["data"], "data") {
		t.Fatalf("binds=%v", binds)
	}
	if _, err := ResolveComposeTierVolumes(tp, "app", []compose.TieredVolume{{Name: "x", Tier: "nope"}}); err == nil {
		t.Fatal("expected error for missing tier")
	}
	// unhealthy tier -> error
	tp.put("cold", "/mnt/cold", "offline")
	if _, err := ResolveComposeTierVolumes(tp, "app", []compose.TieredVolume{{Name: "y", Tier: "cold"}}); err == nil {
		t.Fatal("expected error for unhealthy tier")
	}
}
