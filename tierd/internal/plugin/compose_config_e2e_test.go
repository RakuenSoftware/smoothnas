package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
)

// TestLifecycle_ComposeConfigRendersEnv proves the S2 injection end-to-end:
// non-secret operator config (+ schema defaults) lands in the compose .env at
// Materialise, while a secret goes to the secret store and never touches .env.
func TestLifecycle_ComposeConfigRendersEnv(t *testing.T) {
	store := openTestStore(t)
	inst := NewInstaller(store)
	project := "name: cfg\n" +
		"services:\n  s: { image: nginx }\n" +
		"x-smoothnas:\n" +
		"  config:\n" +
		"    - { key: LLM_ENDPOINT, type: url, default: \"http://def:8080\" }\n" +
		"    - { key: LLM_MODEL, type: string }\n" +
		"    - { key: API_TOKEN, secret: true }\n"
	if _, err := inst.InstallWithOptions([]byte(project), InstallOptions{Config: map[string]string{
		"LLM_MODEL": "custom", "API_TOKEN": "s3cr3t",
	}}); err != nil {
		t.Fatalf("install: %v", err)
	}

	root := t.TempDir()
	lc := NewLifecycle(store, &fakeRuntime{})
	lc.SetComposeBackend(compose.NewBackend(compose.New("", &recRunner{}), root))
	if err := lc.Materialise(context.Background(), "cfg"); err != nil {
		t.Fatalf("materialise: %v", err)
	}

	envBytes, err := os.ReadFile(filepath.Join(root, "cfg", ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	env := string(envBytes)
	if !strings.Contains(env, "LLM_MODEL=custom") { // operator answer
		t.Errorf(".env missing LLM_MODEL=custom:\n%s", env)
	}
	if !strings.Contains(env, "LLM_ENDPOINT=http://def:8080") { // materialized default
		t.Errorf(".env missing defaulted LLM_ENDPOINT:\n%s", env)
	}
	if strings.Contains(env, "API_TOKEN") { // secret must never reach .env
		t.Errorf("secret leaked into .env:\n%s", env)
	}
	secs, err := store.GetComposeSecrets("cfg")
	if err != nil {
		t.Fatalf("GetComposeSecrets: %v", err)
	}
	if secs["API_TOKEN"] != "s3cr3t" {
		t.Errorf("secret not in secret store: %v", secs)
	}
}
