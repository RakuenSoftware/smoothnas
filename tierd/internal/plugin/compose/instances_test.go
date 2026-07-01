package compose

import (
	"testing"

	"gopkg.in/yaml.v3"
)

const ghRunnerProj = `
name: gh-runner
services:
  gh-runner:
    image: ghcr.io/x/gh-runner:1
    x-smoothnas:
      instances: { count: 2, min: 1, max: 8 }
    environment:
      GH_RUNNER_TOKEN: ${GH_RUNNER_TOKEN}
    volumes:
      - "work:/home/runner/_work"
volumes:
  work:
    x-smoothnas: { tier: runner-ssd, perInstance: true, minSize: 100G }
`

func TestScalableServices(t *testing.T) {
	specs, err := ScalableServices([]byte(ghRunnerProj))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Service != "gh-runner" || specs[0].Count != 2 || specs[0].Max != 8 {
		t.Fatalf("specs=%+v", specs)
	}
}

func TestExpandInstances(t *testing.T) {
	out, err := ExpandInstances([]byte(ghRunnerProj), map[string]int{"gh-runner": 3})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]any `yaml:"services"`
		Volumes  map[string]any `yaml:"volumes"`
	}
	mustYAML(t, out, &doc)
	// 3 expanded services, original gone, no template volume left.
	for _, want := range []string{"gh-runner-1", "gh-runner-2", "gh-runner-3"} {
		if _, ok := doc.Services[want]; !ok {
			t.Fatalf("missing service %q in %v", want, keys(doc.Services))
		}
	}
	if _, ok := doc.Services["gh-runner"]; ok {
		t.Fatal("template service should be removed")
	}
	if _, ok := doc.Volumes["work"]; ok {
		t.Fatal("template volume should be removed")
	}
	// Each instance has its own per-instance tiered volume.
	for _, want := range []string{"gh-runner-1-work", "gh-runner-2-work", "gh-runner-3-work"} {
		v, ok := doc.Volumes[want].(map[string]any)
		if !ok {
			t.Fatalf("missing per-instance volume %q in %v", want, keys(doc.Volumes))
		}
		xs := v["x-smoothnas"].(map[string]any)
		if xs["tier"] != "runner-ssd" {
			t.Fatalf("%s tier=%v", want, xs["tier"])
		}
		if _, leaked := xs["perInstance"]; leaked {
			t.Fatalf("%s still carries perInstance flag", want)
		}
	}
	// The instance-1 mount points at its own volume; x-smoothnas.instances gone.
	svc1 := doc.Services["gh-runner-1"].(map[string]any)
	vols := svc1["volumes"].([]any)
	if vols[0].(string) != "gh-runner-1-work:/home/runner/_work" {
		t.Fatalf("instance-1 mount=%v", vols[0])
	}
	if _, ok := svc1["x-smoothnas"]; ok {
		t.Fatal("expanded service should not keep x-smoothnas.instances")
	}
}

func TestExpandInstances_NoScalable(t *testing.T) {
	y := []byte("name: app\nservices:\n  web: { image: nginx }\n")
	out, err := ExpandInstances(y, nil)
	if err != nil || string(out) != string(y) {
		t.Fatalf("non-scalable project must pass through unchanged; out=%q err=%v", out, err)
	}
}

func mustYAML(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := yaml.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
