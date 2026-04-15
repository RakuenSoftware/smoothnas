package mdadm

import (
	"os"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
)

func writeFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func TestSizeBucketRank(t *testing.T) {
	tests := []struct {
		name     string
		size     int64
		fastest  int
		slowest  int
		expected int
	}{
		{"tiny below base", 500 * 1024, 1, 3, 1},
		{"exactly base", 1 << 20, 1, 3, 1},
		{"just under 16MB", (16 << 20) - 1, 1, 3, 1},
		{"exactly 16MB", 16 << 20, 1, 3, 2},
		{"100MB", 100 << 20, 1, 3, 2},
		{"just under 256MB", (256 << 20) - 1, 1, 3, 2},
		{"exactly 256MB", 256 << 20, 1, 3, 3},
		{"1GB", 1 << 30, 1, 3, 3},
		{"1TB clamps to slowest", 1 << 40, 1, 3, 3},

		{"2-tier small", 100 * 1024, 1, 2, 1},
		{"2-tier large", 100 << 20, 1, 2, 2},

		{"zero-sized", 0, 1, 3, 1},
		{"negative", -1, 1, 3, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sizeBucketRank(tt.size, tt.fastest, tt.slowest)
			if got != tt.expected {
				t.Errorf("sizeBucketRank(%d, %d, %d) = %d, want %d",
					tt.size, tt.fastest, tt.slowest, got, tt.expected)
			}
		})
	}
}

func TestIdealRank_PinOverridesSize(t *testing.T) {
	// Large file (would otherwise go to slowest tier) pinned hot → fastest.
	got := idealRank(meta.PinHot, 10<<30, 1, 3)
	if got != 1 {
		t.Errorf("PinHot on 10GB file: got rank %d, want 1 (fastest)", got)
	}
	// Tiny file (would otherwise go to fastest) pinned cold → slowest.
	got = idealRank(meta.PinCold, 1024, 1, 3)
	if got != 3 {
		t.Errorf("PinCold on 1KB file: got rank %d, want 3 (slowest)", got)
	}
	// Unpinned medium file: size bucket wins.
	got = idealRank(meta.PinNone, 100<<20, 1, 3)
	if got != 2 {
		t.Errorf("PinNone on 100MB file: got rank %d, want 2", got)
	}
}

// --- bin-packing admission --------------------------------------------------

type testRanking struct {
	rank      int
	targetPct int
	fullPct   int
}

func testRanked(in ...testRanking) []rankedPoolTarget {
	out := make([]rankedPoolTarget, 0, len(in))
	for _, r := range in {
		out = append(out, rankedPoolTarget{
			rank:             r.rank,
			targetFillPct:    r.targetPct,
			fullThresholdPct: r.fullPct,
		})
	}
	return out
}

// TestAdmitFillsFastestFirst: empty fastest tier accepts small files
// until target_fill is hit, then spills large files. This is the
// "prefer higher when we can fit it" property.
func TestAdmitFillsFastestFirst(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 1, 1<<20); r != 1 {
		t.Errorf("1MB on empty fastest: got rank %d, want 1", r)
	}
	if r := admitWithFallback(caps, ranked, 1, 1<<30); r != 2 {
		t.Errorf("1GB past 512MB target: got rank %d, want 2", r)
	}
}

// TestAdmitFallsToFullCap: every tier past target fill; full-threshold
// fallback still admits.
func TestAdmitFallsToFullCap(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: 1 << 29, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 1, 100<<20); r != 2 {
		t.Errorf("past-target fastest: got rank %d, want 2", r)
	}
	caps[2].usedBytes = caps[2].targetCap
	if r := admitWithFallback(caps, ranked, 1, 10<<20); r != 1 {
		t.Errorf("target busted, fullCap fallback: got rank %d, want 1", r)
	}
}

func TestAdmitRejectsOversizedFile(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 1 << 30, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 1, 10<<30); r != 1 {
		t.Errorf("oversized: got rank %d, want 1", r)
	}
	if caps[1].usedBytes != 0 || caps[2].usedBytes != 0 {
		t.Error("rejected admission mutated caps")
	}
}

// TestAdmitPinColdStartsFromSlowest: PinCold passes slowestRank as
// preferredRank and does not back up to faster tiers.
func TestAdmitPinColdStartsFromSlowest(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 2, 1<<20); r != 2 {
		t.Errorf("PinCold small: got rank %d, want 2", r)
	}
}

// TestHeatDecayHalves verifies decayAllHeat halves HeatCounter values via
// the Iterate+PutBlocking path and preserves other fields.
func TestHeatDecayHalves(t *testing.T) {
	dir := t.TempDir()
	store, err := meta.Open([]meta.TierBacking{{Rank: 1, Name: "test", BackingMount: dir}})
	if err != nil {
		t.Fatalf("open meta: %v", err)
	}
	defer store.Close()

	// Seed a handful of records across a spread of heat counters.
	inputs := []struct {
		inode uint64
		heat  uint32
		pin   meta.PinState
	}{
		{inode: 1, heat: 0, pin: meta.PinNone},
		{inode: 2, heat: 1, pin: meta.PinHot},
		{inode: 3, heat: 100, pin: meta.PinNone},
		{inode: 4, heat: 1_000_000, pin: meta.PinCold},
	}
	for _, in := range inputs {
		store.PutBlocking(in.inode, meta.Record{
			Version:     meta.RecordVersion,
			PinState:    in.pin,
			TierIdx:     1,
			HeatCounter: in.heat,
		})
	}

	// Flush the seed writes (close+reopen) so Iterate sees them when the
	// decay pass scans.
	if err := store.Close(); err != nil {
		t.Fatalf("close (pre-decay): %v", err)
	}
	store, err = meta.Open([]meta.TierBacking{{Rank: 1, Name: "test", BackingMount: dir}})
	if err != nil {
		t.Fatalf("reopen (pre-decay): %v", err)
	}

	// Build a minimal Adapter with just the meta-stores map populated.
	a := &Adapter{}
	a.metaStores = map[string]*meta.PoolMetaStore{"testpool": store}
	a.decayAllHeat()

	// Flush again so the decay's async writes committed by decayAllHeat
	// are readable.
	if err := store.Close(); err != nil {
		t.Fatalf("close (post-decay): %v", err)
	}
	store, err = meta.Open([]meta.TierBacking{{Rank: 1, Name: "test", BackingMount: dir}})
	if err != nil {
		t.Fatalf("reopen (post-decay): %v", err)
	}
	defer store.Close()

	for _, in := range inputs {
		got, ok, err := store.Get(in.inode)
		if err != nil || !ok {
			t.Fatalf("inode %d: missing after decay (ok=%v err=%v)", in.inode, ok, err)
		}
		want := in.heat / 2
		if got.HeatCounter != want {
			t.Errorf("inode %d HeatCounter = %d, want %d", in.inode, got.HeatCounter, want)
		}
		if got.PinState != in.pin {
			t.Errorf("inode %d PinState drifted: got %d, want %d", in.inode, got.PinState, in.pin)
		}
	}
}

// TestCopyFileContents exercises the low-level file-copy helper used by
// the movement executor. Full end-to-end move tests require building a
// real Adapter with a DB, tier targets, and an open namespace — out of
// scope for unit tests here; covered by live-deploy verification.
func TestCopyFileContents(t *testing.T) {
	dir := t.TempDir()
	src := dir + "/src"
	dst := dir + "/dst"
	if err := writeFile(src, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := copyFileContents(src, dst, 0o644); err != nil {
		t.Fatalf("copy: %v", err)
	}
	got, err := readFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("dst contents = %q, want %q", got, "hello world")
	}
}
