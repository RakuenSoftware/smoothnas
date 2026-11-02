package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
	"gopkg.in/yaml.v3"
)

func pbsProject(t *testing.T) []byte {
	t.Helper()
	b, err := localPluginsFS.ReadFile("localplugins/pbs/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Execute the actual bootstrap embedded in the shipped Compose config, after
// Compose's $$ escaping, against a temporary configuration directory. No PBS
// server or network is involved, and the public upstream password is never used.
func TestPBSBootstrap(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("Perl unavailable")
	}
	var doc struct {
		Configs map[string]struct {
			Content string `yaml:"content"`
		} `yaml:"configs"`
	}
	if err := yaml.Unmarshal(pbsProject(t), &doc); err != nil {
		t.Fatal(err)
	}
	command := strings.ReplaceAll(doc.Configs["bootstrap"].Content, "$$", "$")
	_, rest, ok := strings.Cut(command, "<<'PERL'\n")
	if !ok {
		t.Fatal("missing bootstrap")
	}
	script, _, ok := strings.Cut(rest, "\nPERL\n")
	if !ok {
		t.Fatal("missing bootstrap terminator")
	}
	run := func(dir, password string) error {
		cmd := exec.Command("bash", "-c", `umask 077; exec perl - "$1"`, "bootstrap", dir)
		cmd.Env = append(os.Environ(), "PBS_ADMIN_PASSWORD="+password)
		cmd.Stdin = strings.NewReader(script)
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), password) && password != "" {
			t.Fatal("bootstrap leaked password")
		}
		return err
	}
	for _, password := range []string{"", "short", "long-password\nnewline"} {
		dir := t.TempDir()
		if err := run(dir, password); err == nil {
			t.Fatalf("accepted invalid password %q", password)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatal("invalid password wrote configuration")
		}
	}
	dir := t.TempDir()
	password := "correct horse $quotes'\"\\ battery"
	if err := run(dir, password); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	shadowPath := filepath.Join(dir, "shadow.json")
	before, err := os.ReadFile(shadowPath)
	if err != nil {
		t.Fatal(err)
	}
	var shadow map[string]string
	if err := json.Unmarshal(before, &shadow); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(shadow["admin"], "$6$") {
		t.Fatal("missing SHA-512 password hash")
	}
	verify := exec.Command("perl", "-e", `exit(crypt($ENV{PASSWORD}, $ENV{HASH}) eq $ENV{HASH} ? 0 : 1)`)
	verify.Env = append(os.Environ(), "PASSWORD="+password, "HASH="+shadow["admin"])
	if err := verify.Run(); err != nil {
		t.Fatal("stored password does not authenticate")
	}
	info, _ := os.Stat(shadowPath)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("shadow mode = %v", info.Mode())
	}
	// Reattach/restart preserves subsequent PBS password changes, even when the
	// install wizard supplies a different initial password or no password.
	for _, password := range []string{"", "different-password"} {
		if err := run(dir, password); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(shadowPath)
		if string(after) != string(before) {
			t.Fatal("restart reset existing credentials")
		}
	}
	imported := t.TempDir()
	if err := os.WriteFile(filepath.Join(imported, "user.cfg"), []byte("imported"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(imported, "new-valid-password"); err == nil {
		t.Fatal("overwrote imported configuration")
	}
}

func TestPBSLocalCatalogAndWebUI(t *testing.T) {
	m, err := plugin.ParseManifest(pbsProject(t))
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsCompose() || m.UI != nil || m.WebUI == nil || m.WebUI.Scheme != "https" || m.WebUI.Port != 8007 {
		t.Fatalf("PBS must open directly over HTTPS, without an iframe: %+v", m)
	}
	h, installer := newPluginsHandlerForTest(t)
	rec, err := installer.InstallWithOptions(pbsProject(t), plugin.InstallOptions{Config: map[string]string{"PBS_ADMIN_PASSWORD": "initial-test-password"}})
	if err != nil {
		t.Fatal(err)
	}
	view := toPluginListItem(rec)
	if view.WebUI == nil || view.WebUI.Port != 8007 {
		t.Fatal("Open link missing from API")
	}
	if strings.Contains(rec.Plugin.ManifestYAML, "initial-test-password") {
		t.Fatal("secret written into manifest")
	}
	secrets, err := h.store.GetComposeSecrets("pbs")
	if err != nil || secrets["PBS_ADMIN_PASSWORD"] != "initial-test-password" {
		t.Fatal("initial password not stored as a secret")
	}
}
