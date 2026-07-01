package compose

import (
	"strings"
	"testing"
)

func TestTieredVolumes(t *testing.T) {
	y := `
name: app
services:
  web: { image: nginx, volumes: ["data:/var/data", "cache:/cache"] }
volumes:
  data:
    x-smoothnas: { tier: fast, minSize: 10G }
  cache: {}                       # ordinary, not tiered
  logs:
    x-smoothnas: { tier: bulk }
`
	got, err := TieredVolumes([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "data" || got[0].Tier != "fast" || got[0].MinSize != "10G" || got[1].Name != "logs" || got[1].Tier != "bulk" {
		t.Fatalf("got %+v", got)
	}
}

func TestRewriteTieredBinds(t *testing.T) {
	y := "name: app\nservices:\n  web:\n    image: nginx\n    volumes:\n      - \"data:/var/data:ro\"\n      - \"cache:/cache\"\n" +
		"volumes:\n  data:\n    x-smoothnas: { tier: fast }\n  cache: {}\n"
	out, err := RewriteTieredBinds([]byte(y), map[string]string{"data": "/mnt/fast/app/data"})
	if err != nil {
		t.Fatal(err)
	}
	so := string(out)
	if !strings.Contains(so, "/mnt/fast/app/data:/var/data:ro") {
		t.Fatalf("data mount not rewritten to bind:\n%s", so)
	}
	if !strings.Contains(so, "cache:/cache") {
		t.Fatalf("non-tiered mount should be untouched:\n%s", so)
	}
	if strings.Contains(so, "x-smoothnas") {
		t.Fatalf("tiered volume def should be dropped from top-level volumes:\n%s", so)
	}
	// no-op passthrough
	if b, _ := RewriteTieredBinds([]byte(y), nil); string(b) != y {
		t.Fatal("nil binds must passthrough unchanged")
	}
}

func TestSetVolumeTiers(t *testing.T) {
	y := "volumes:\n  data:\n    x-smoothnas: { tier: default-pool }\n  other: {}\n"
	out, err := SetVolumeTiers([]byte(y), map[string]string{"data": "fast-pool", "other": "x", "missing": "y"})
	if err != nil {
		t.Fatal(err)
	}
	tv, _ := TieredVolumes(out)
	if len(tv) != 1 || tv[0].Name != "data" || tv[0].Tier != "fast-pool" {
		t.Fatalf("tier override failed: %+v", tv)
	}
	// no assignments => passthrough
	if b, _ := SetVolumeTiers([]byte(y), nil); string(b) != y {
		t.Fatal("nil tiers must passthrough")
	}
}
