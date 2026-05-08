package mdadm

import (
	"os"
	"strings"
	"testing"

	smoothfsclient "github.com/RakuenSoftware/smoothfs"
	"github.com/google/uuid"
)

// TestMountSemanticsLines locks the comparison logic that decides
// whether a unit-body change can be applied with a soft rewrite or
// requires destroy+recreate. The four mount() arguments — What,
// Where, Type, Options — are the only fields the kernel mount
// syscall actually consumes; everything else is metadata.
func TestMountSemanticsLines(t *testing.T) {
	withTimeout := `[Unit]
Description=smoothfs pool media
[Mount]
What=none
Where=/mnt/media
Type=smoothfs
Options=pool=media,uuid=00000000-0000-0000-0000-000000000001,tiers=/a:/b
TimeoutSec=infinity
[Install]
WantedBy=local-fs.target
`
	withoutTimeout := `[Unit]
Description=smoothfs pool media
[Mount]
What=none
Where=/mnt/media
Type=smoothfs
Options=pool=media,uuid=00000000-0000-0000-0000-000000000001,tiers=/a:/b
[Install]
WantedBy=local-fs.target
`
	a, aok := mountSemanticsLines(withTimeout)
	b, bok := mountSemanticsLines(withoutTimeout)
	if !aok || !bok || a != b {
		t.Fatalf("TimeoutSec drift misclassified: got=(%q,%v) want=(%q,%v)", a, aok, b, bok)
	}

	differentTiers := strings.Replace(withTimeout, "tiers=/a:/b", "tiers=/a:/b:/c", 1)
	c, cok := mountSemanticsLines(differentTiers)
	if !cok || a == c {
		t.Fatalf("Options= drift not detected: got %q vs %q", a, c)
	}

	differentDescription := strings.Replace(withTimeout, "Description=smoothfs pool media", "Description=changed", 1)
	d, dok := mountSemanticsLines(differentDescription)
	if !dok || a != d {
		t.Fatalf("Description= drift wrongly classified as a mount-semantics change: got %q vs %q", a, d)
	}

	noMountSection := `[Unit]
Description=foo
[Install]
WantedBy=local-fs.target
`
	if _, ok := mountSemanticsLines(noMountSection); ok {
		t.Fatal("missing [Mount] should report ok=false")
	}
}

// TestSmoothfsUnitMountSemanticsMatchTemplateChurn is the regression
// gate for the 0.0.48 update: when the smoothfs library bumped from
// no-TimeoutSec to TimeoutSec=infinity, the entire body changed and
// `smoothfsUnitMatches` returned false, but the kernel mount itself
// was still semantically correct. This asserts the soft path kicks
// in for that exact change.
func TestSmoothfsUnitMountSemanticsMatchTemplateChurn(t *testing.T) {
	dir := t.TempDir()
	unit := dir + "/mnt-media.mount"

	pool := smoothfsclient.ManagedPool{
		Name:       "media",
		UUID:       uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Tiers:      []string{mkTier(t, dir, "fast"), mkTier(t, dir, "slow")},
		Mountpoint: "/mnt/media",
		UnitPath:   unit,
	}

	// "Old" file body: identical to what the current renderer produces,
	// minus the TimeoutSec= line that smoothfs#120 added.
	rendered, err := renderSmoothfsUnit(pool)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	old := strings.ReplaceAll(rendered, "TimeoutSec=infinity\n", "")
	if old == rendered {
		t.Fatalf("renderer produced no TimeoutSec=infinity line — test stub is stale, fix here:\n%s", rendered)
	}
	if err := writeFileAtomic(unit, old); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	// Whole-body match should return false.
	if matches, err := smoothfsUnitMatches(pool); err != nil || matches {
		t.Fatalf("smoothfsUnitMatches(churn) = (%v, %v), want (false, nil)", matches, err)
	}
	// But mount semantics should still match.
	if same, err := smoothfsUnitMountSemanticsMatch(pool); err != nil || !same {
		t.Fatalf("smoothfsUnitMountSemanticsMatch(churn) = (%v, %v), want (true, nil)", same, err)
	}
}

// TestSmoothfsUnitMountSemanticsMatchTierChange asserts that when the
// tier list shifts (the actual mount() Options change), the soft path
// is rejected so destroy+recreate runs.
func TestSmoothfsUnitMountSemanticsMatchTierChange(t *testing.T) {
	dir := t.TempDir()
	unit := dir + "/mnt-media.mount"

	pool := smoothfsclient.ManagedPool{
		Name:       "media",
		UUID:       uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Tiers:      []string{mkTier(t, dir, "fast"), mkTier(t, dir, "slow")},
		Mountpoint: "/mnt/media",
		UnitPath:   unit,
	}

	// Seed an on-disk unit with a *different* tier list. Real-world
	// trigger: operator added or removed a tier.
	stale := pool
	stale.Tiers = []string{mkTier(t, dir, "only-fast")}
	staleBody, err := renderSmoothfsUnit(stale)
	if err != nil {
		t.Fatalf("render stale: %v", err)
	}
	if err := writeFileAtomic(unit, staleBody); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	if same, err := smoothfsUnitMountSemanticsMatch(pool); err != nil || same {
		t.Fatalf("smoothfsUnitMountSemanticsMatch(tier-change) = (%v, %v), want (false, nil)", same, err)
	}
}

func writeFileAtomic(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

func mkTier(t *testing.T, parent, name string) string {
	t.Helper()
	p := parent + "/" + name
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	return p
}
