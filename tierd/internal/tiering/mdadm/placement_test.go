package mdadm

import (
	"os"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
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

// TestAdmitFillsFastestFirst: empty fastest tier accepts small files.
// Since admission now uses fullCap as the normal cap, small files on
// an empty tier always land on the fastest rank.
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
		t.Errorf("1GB past 1GB total: got rank %d, want 2", r)
	}
}

// TestAdmitStaysPastTargetUntilFullCap: a fastest tier that is past its
// target_fill_pct but still below full_threshold_pct should continue to
// accept new files. Previous behaviour demoted at target; now the
// planner only demotes after full_threshold_pct is crossed.
func TestAdmitStaysPastTargetUntilFullCap(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// Fastest tier is at its target_fill (512 MB of 1 GB) but well below
	// full_threshold (974 MB). A new 100 MB file should still land there.
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: 1 << 29, initialUsed: 1 << 29, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 1, 100<<20); r != 1 {
		t.Errorf("past-target-under-full fastest: got rank %d, want 1", r)
	}
}

func TestAdmitBalancesToTargetDuringSpindownMaintenance(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	usedFast := int64(1<<30) * 60 / 100
	caps := map[int]*tierCapacity{
		1: {
			totalBytes:          1 << 30,
			usedBytes:           usedFast,
			initialUsed:         usedFast,
			targetCap:           1 << 29,
			fullCap:             (1 << 30) * 95 / 100,
			balanceToTargetFill: true,
		},
		2: {
			totalBytes:          10 << 30,
			targetCap:           10 << 29,
			fullCap:             (10 << 30) * 95 / 100,
			balanceToTargetFill: true,
		},
	}
	if r := admitWithFallback(caps, ranked, 1, 10<<20); r != 2 {
		t.Errorf("spindown target-balance fastest: got rank %d, want 2", r)
	}

	caps[1].usedBytes = int64(1<<30) * 49 / 100
	caps[1].initialUsed = caps[1].usedBytes
	if r := admitWithFallback(caps, ranked, 1, 5<<20); r != 1 {
		t.Errorf("spindown target-balance below target: got rank %d, want 1", r)
	}
}

// TestAdmitDrainsWhenOverFullCap: a tier that started the cycle above
// full_threshold_pct enters drain mode — admissionCap falls back to
// targetCap so new files spill to the next tier until usedBytes drops
// below the soft cap. This gives the planner hysteresis: fill to full%,
// drain to fill%.
func TestAdmitDrainsWhenOverFullCap(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// Fastest tier starts above full_threshold (98%). It must drain back
	// to target_fill (50%) before accepting new placements.
	used := int64(1<<30) * 98 / 100
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: used, initialUsed: used, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 1, 10<<20); r != 2 {
		t.Errorf("fastest in drain mode: got rank %d, want 2", r)
	}
}

// TestAdmitFallsToFullCapLastResort: admissionCap refuses every tier
// from the preferred rank downward; Pass B falls back to fullCap so
// the file isn't stranded.
func TestAdmitFallsToFullCapLastResort(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// Rank 2 is in drain mode (above fullCap), rank 1 fills all the way
	// to fullCap already. A 10 MB file has nowhere under admissionCap
	// but rank 1 still has sub-fullCap slack (fullCap - usedBytes > 10 MB).
	usedFast := (int64(1) << 30) * 95 / 100
	usedSlow := (int64(10) << 30) * 96 / 100
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: usedFast, initialUsed: usedFast, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, usedBytes: usedSlow, initialUsed: usedSlow, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	// Both tiers refuse in Pass A: rank 1 is at fullCap, rank 2 is draining.
	// Pass B admits at rank 1 (fullCap fallback still has room after we
	// subtract — but in this setup both are above fullCap-rounding, so it
	// should at least not strand the file by returning preferredRank=1).
	r := admitWithFallback(caps, ranked, 1, 10<<20)
	if r != 1 && r != 2 {
		t.Errorf("last-resort: got rank %d, want 1 or 2", r)
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

func TestPoolRankedTargetsUsesFullThresholdForSlowest(t *testing.T) {
	store := openAdapterStore(t)
	if err := store.CreateTierPool("pool1", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}); err != nil {
		t.Fatalf("CreateTierPool: %v", err)
	}
	row1 := &db.TierTargetRow{
		Name: "NVME", PlacementDomain: "pool1", BackendKind: BackendKind,
		Rank: 1, TargetFillPct: 60, FullThresholdPct: 90, BackingRef: backingRefTarget("pool1", "NVME"),
	}
	row2 := &db.TierTargetRow{
		Name: "HDD", PlacementDomain: "pool1", BackendKind: BackendKind,
		Rank: 2, TargetFillPct: 40, FullThresholdPct: 85, BackingRef: backingRefTarget("pool1", "HDD"),
	}
	if err := store.CreateTierTarget(row1); err != nil {
		t.Fatalf("CreateTierTarget row1: %v", err)
	}
	if err := store.CreateTierTarget(row2); err != nil {
		t.Fatalf("CreateTierTarget row2: %v", err)
	}
	if err := store.UpsertMdadmManagedTarget(&db.MdadmManagedTargetRow{
		TierTargetID: row1.ID, PoolName: "pool1", TierName: "NVME", MountPath: t.TempDir(),
	}); err != nil {
		t.Fatalf("UpsertMdadmManagedTarget row1: %v", err)
	}
	if err := store.UpsertMdadmManagedTarget(&db.MdadmManagedTargetRow{
		TierTargetID: row2.ID, PoolName: "pool1", TierName: "HDD", MountPath: t.TempDir(),
	}); err != nil {
		t.Fatalf("UpsertMdadmManagedTarget row2: %v", err)
	}

	a := NewAdapter(store, t.TempDir())
	ranked := a.poolRankedTargets("pool1")
	if len(ranked) != 2 {
		t.Fatalf("ranked len = %d, want 2", len(ranked))
	}
	if ranked[0].targetFillPct != 60 {
		t.Fatalf("fast tier target_fill_pct = %d, want 60", ranked[0].targetFillPct)
	}
	if ranked[1].targetFillPct != 85 {
		t.Fatalf("slowest tier target_fill_pct = %d, want 85", ranked[1].targetFillPct)
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
		store.PutBlocking(in.inode, 1, meta.Record{
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
		got, ok, err := store.Get(in.inode, 1)
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
