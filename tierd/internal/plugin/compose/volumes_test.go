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

func TestBindOverride(t *testing.T) {
	out, err := BindOverride(map[string]string{"data": "/mnt/fast/app/data"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"driver: local", "type: none", "o: bind", "device: /mnt/fast/app/data", "com.smoothnas.tiered"} {
		if !strings.Contains(out, want) {
			t.Fatalf("override missing %q:\n%s", want, out)
		}
	}
	if s, _ := BindOverride(nil); s != "" {
		t.Fatalf("empty binds => empty override, got %q", s)
	}
}
