package mdadm

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestPlacementExcludedDirSkipsRootInternalDirsOnly(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name string
		path string
		dir  string
		want bool
	}{
		{"root smoothnas", root + "/.smoothnas", ".smoothnas", true},
		{"root plugins", root + "/.plugins", ".plugins", true},
		{"root smoothfs", root + "/.smoothfs", ".smoothfs", true},
		{"root tierd meta", root + "/.tierd-meta", ".tierd-meta", true},
		{"nested smoothnas is user data", root + "/storage/.smoothnas", ".smoothnas", false},
		{"nested plugins is user data", root + "/storage/.plugins", ".plugins", false},
		{"lost found remains excluded", root + "/storage/lost+found", "lost+found", true},
		{"regular directory", root + "/storage", "storage", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := placementExcludedDir(root, tt.path, tt.dir); got != tt.want {
				t.Fatalf("placementExcludedDir() = %v, want %v", got, tt.want)
			}
		})
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

// TestAdmitFillsFastestFirst: empty fastest tier accepts small files until
// it reaches target_fill_pct; larger files then spill to the lower tier.
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

func TestAdmitSpillsPastTargetFill(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// Fastest tier is at target_fill_pct — the admission ceiling — so the file
	// spills to the next slower tier. fill% is the migration trigger; full% is
	// only the smoothfs write-admission ceiling (not the planner's trigger).
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: 1 << 29, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 1, 100<<20); r != 2 {
		t.Errorf("at-target-fill fastest: got rank %d, want 2", r)
	}
}

func TestAdmitUsesTargetFill(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// At 60% — above targetCap (50%) — file spills to slower tier.
	caps := map[int]*tierCapacity{
		1: {
			totalBytes: 1 << 30,
			usedBytes:  int64(1<<30) * 60 / 100,
			targetCap:  1 << 29,
			fullCap:    (1 << 30) * 95 / 100,
		},
		2: {
			totalBytes: 10 << 30,
			targetCap:  10 << 29,
			fullCap:    (10 << 30) * 95 / 100,
		},
	}
	if r := admitWithFallback(caps, ranked, 1, 10<<20); r != 2 {
		t.Errorf("above target-fill fastest: got rank %d, want 2", r)
	}

	// At 49% — below targetCap — file fits on the fast tier.
	caps[1].usedBytes = int64(1<<30) * 49 / 100
	if r := admitWithFallback(caps, ranked, 1, 5<<20); r != 1 {
		t.Errorf("below target-fill: got rank %d, want 1", r)
	}
}

func TestAdmitDrainsWhenOverTargetFill(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// 80% > targetCap (50%) → file must spill to slower tier.
	used := int64(1<<30) * 80 / 100
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: used, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 1, 10<<20); r != 2 {
		t.Errorf("fastest above target_fill: got rank %d, want 2", r)
	}
}

func TestAdmitFullFallbackDrainsToSlowestEligibleTier(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// Tier 1 is above targetCap; tier 2 has headroom.
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 600, targetCap: 500, fullCap: 950},
		2: {totalBytes: 1000, usedBytes: 500, targetCap: 500, fullCap: 950},
	}

	if r := admitWithFallback(caps, ranked, 1, 100); r != 2 {
		t.Errorf("above target_fill: got rank %d, want 2", r)
	}
	if caps[1].usedBytes != 600 {
		t.Errorf("upper tier usedBytes = %d, want 600 (unchanged)", caps[1].usedBytes)
	}
	if caps[2].usedBytes != 600 {
		t.Errorf("bottom tier usedBytes = %d, want 600", caps[2].usedBytes)
	}
}

func TestAssignCandidateRanksDrainsUpperTierTowardTargetFill(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// targetCap for tier 1 is 500, so the second 400-byte file pushes past it
	// and must be assigned to tier 2.
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 2000, usedBytes: 0, targetCap: 1000, fullCap: 1900},
	}
	cands := []candidate{
		{curRank: 1, size: 400, pin: meta.PinNone},
		{curRank: 1, size: 400, pin: meta.PinNone},
	}

	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	for i := range cands {
		if !assigned[i] {
			t.Fatalf("candidate %d was not assigned", i)
		}
	}
	if assignments[0] != 1 {
		t.Fatalf("candidate 0 assignment = %d, want 1", assignments[0])
	}
	if assignments[1] != 2 {
		t.Fatalf("candidate 1 assignment = %d, want 2 (tier 1 targetCap exceeded)", assignments[1])
	}
	if caps[1].usedBytes != 400 {
		t.Fatalf("upper tier usedBytes = %d, want 400", caps[1].usedBytes)
	}
}

// TestAssignCandidateRanksPromotesFileFromHDDWhenFastTierHasRoom verifies that
// an unpinned file on a slow tier is promoted to the fast tier when the fast
// tier has capacity below full_threshold_pct. All unpinned files start from
// fastestRank; hot/small files win fast-tier capacity first.
func TestAssignCandidateRanksPromotesFileFromHDDWhenFastTierHasRoom(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 2000, usedBytes: 0, targetCap: 1000, fullCap: 1900},
	}
	// File on tier 2 (HDD). Tier 1 has plenty of room under fullCap — promote.
	cands := []candidate{
		{curRank: 2, size: 100, pin: meta.PinNone},
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 1 {
		t.Errorf("HDD file assigned to rank %d, want 1 (promoted to fast tier)", assignments[0])
	}
	if caps[1].usedBytes != 100 {
		t.Errorf("tier 1 usedBytes = %d, want 100", caps[1].usedBytes)
	}
}

// TestAssignCandidateRanksDoesNotPromoteWhenFastTierAtTargetFill verifies that a
// file on the slow tier stays there when the fast tier is at or above
// target_fill_pct. fill% is the admission ceiling for the planner.
func TestAssignCandidateRanksDoesNotPromoteWhenFastTierAtTargetFill(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 500, targetCap: 500, fullCap: 950},
		2: {totalBytes: 2000, usedBytes: 0, targetCap: 1000, fullCap: 1900},
	}
	// Tier 1 is at targetCap (500/500) — no room to promote; file stays on tier 2.
	cands := []candidate{
		{curRank: 2, size: 100, pin: meta.PinNone},
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 2 {
		t.Errorf("file promoted to rank %d despite tier at target fill, want 2", assignments[0])
	}
}

// TestAssignCandidateRanksHeatTrumpsSize verifies that a hot large file beats
// a cold small file for fast-tier capacity. Heat is the primary sort key; size
// is secondary and only breaks ties among files with equal heat.
func TestAssignCandidateRanksHeatTrumpsSize(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// Tier 1 targetCap = 400 bytes — exactly fits the hot file but not both together.
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 400, fullCap: 950},
		2: {totalBytes: 2000, usedBytes: 0, targetCap: 1000, fullCap: 1900},
	}
	// cold small file (size 50) vs hot large file (size 400, heat 10).
	// Hot file wins tier 1 even though it is bigger; cold file must go to tier 2.
	cands := []candidate{
		{curRank: 2, size: 50, pin: meta.PinNone, heat: 0},   // cold, small — index 0
		{curRank: 2, size: 400, pin: meta.PinNone, heat: 10}, // hot, large — index 1
	}
	assignments, _ := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if assignments[1] != 1 {
		t.Errorf("hot large file assigned to rank %d, want 1 (heat trumps size)", assignments[1])
	}
	if assignments[0] != 2 {
		t.Errorf("cold small file assigned to rank %d, want 2 (lost to hot file)", assignments[0])
	}
}

func TestAdmitPinnedHotFullFallbackKeepsFastestEligibleTier(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 600, targetCap: 500, fullCap: 950},
		2: {totalBytes: 1000, usedBytes: 500, targetCap: 500, fullCap: 950},
	}

	if r := admitWithFallbackOrder(caps, ranked, 1, 100, fullFallbackFastestFirst); r != 1 {
		t.Errorf("pinned-hot fallback: got rank %d, want 1", r)
	}
}

// TestAdmitFallsToFullCapLastResort: admissionCap refuses every tier
// from the preferred rank downward; Pass B falls back to fullCap so
// the file isn't stranded.
func TestAdmitFallsToFullCapLastResort(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// Rank 2 is in drain mode (above fullCap), and rank 1 is already at
	// fullCap. A 10 MB file has nowhere under either admissionCap or fullCap.
	usedFast := (int64(1) << 30) * 95 / 100
	usedSlow := (int64(10) << 30) * 96 / 100
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: usedFast, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, usedBytes: usedSlow, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	// Both tiers refuse in Pass A and Pass B, so the planner returns the
	// preferred rank unchanged and leaves the file in place.
	r := admitWithFallback(caps, ranked, 1, 10<<20)
	if r != 1 {
		t.Errorf("last-resort: got rank %d, want 1", r)
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

func TestAssignCandidateRanksKeepsDuplicateInodesIndependent(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, targetCap: 500, fullCap: 950},
		2: {totalBytes: 2000, targetCap: 1000, fullCap: 1900},
	}
	cands := []candidate{
		{inode: 42, curRank: 1, size: 400, pin: meta.PinNone},
		{inode: 42, curRank: 2, size: 700, pin: meta.PinNone},
	}

	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	for i := range cands {
		if !assigned[i] {
			t.Fatalf("candidate %d was not assigned", i)
		}
	}
	if assignments[0] != 1 {
		t.Fatalf("candidate 0 assignment = %d, want 1", assignments[0])
	}
	if assignments[1] != 2 {
		t.Fatalf("candidate 1 assignment = %d, want 2", assignments[1])
	}
}

func TestPoolRankedTargetsPreservesTargetFillForSlowest(t *testing.T) {
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
	if ranked[1].targetFillPct != 40 {
		t.Fatalf("slowest tier target_fill_pct = %d, want 40", ranked[1].targetFillPct)
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

func TestCopyFileContentsDoesNotOverwriteExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := dir + "/src"
	dst := dir + "/dst"
	if err := writeFile(src, []byte("new"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := writeFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if err := copyFileContents(src, dst, 0o644); err == nil {
		t.Fatal("copyFileContents succeeded over an existing destination")
	}
	got, err := readFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("dst contents = %q, want old", got)
	}
}

func TestMoveForPlacementDoesNotOverwriteExistingDestination(t *testing.T) {
	origMountReady := backingMountActive
	backingMountActive = func(string) bool { return true }
	t.Cleanup(func() { backingMountActive = origMountReady })

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	rel := "dir/file.txt"
	if err := os.MkdirAll(srcDir+"/dir", 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.MkdirAll(dstDir+"/dir", 0o755); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	if err := writeFile(srcDir+"/"+rel, []byte("source"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := writeFile(dstDir+"/"+rel, []byte("dest"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	a := &Adapter{}
	err := a.moveForPlacement(
		db.MdadmManagedNamespaceRow{PoolName: "pool1", NamespaceID: "ns1"},
		rel,
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "src", MountPath: srcDir},
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "dst", MountPath: dstDir},
		1,
		2,
	)
	if err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("moveForPlacement error = %v, want destination exists", err)
	}
	gotSrc, err := readFile(srcDir + "/" + rel)
	if err != nil {
		t.Fatalf("read src: %v", err)
	}
	if string(gotSrc) != "source" {
		t.Fatalf("src contents = %q, want source", gotSrc)
	}
	gotDst, err := readFile(dstDir + "/" + rel)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(gotDst) != "dest" {
		t.Fatalf("dst contents = %q, want dest", gotDst)
	}
}

// After moving a file out-of-band (copy + os.Remove of the source), tierd must
// tell smoothfs to drop the replay pin on the removed source inode, else its
// backing blocks leak until unmount. This asserts the forget_lower call fires
// with the correct pool, source tier rank, and the source's lower inode number.
func TestMoveForPlacementForgetsLowerInodeAfterMove(t *testing.T) {
	origMountReady := backingMountActive
	backingMountActive = func(string) bool { return true }
	t.Cleanup(func() { backingMountActive = origMountReady })

	type forgetCall struct {
		pool string
		tier string
		ino  uint64
	}
	var calls []forgetCall
	origForget := forgetLowerInode
	forgetLowerInode = func(_ *Adapter, poolName string, tierMountPath string, lowerIno uint64) {
		calls = append(calls, forgetCall{poolName, tierMountPath, lowerIno})
	}
	t.Cleanup(func() { forgetLowerInode = origForget })

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	rel := "dir/file.txt"
	if err := os.MkdirAll(srcDir+"/dir", 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := writeFile(srcDir+"/"+rel, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	// Capture the source's lower inode before the move unlinks it.
	fi, err := os.Stat(srcDir + "/" + rel)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	srcIno := fi.Sys().(*syscall.Stat_t).Ino

	a := &Adapter{}
	if err := a.moveForPlacement(
		db.MdadmManagedNamespaceRow{PoolName: "pool1", NamespaceID: "ns1"},
		rel,
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "src", MountPath: srcDir},
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "dst", MountPath: dstDir},
		1, 2,
	); err != nil {
		t.Fatalf("moveForPlacement: %v", err)
	}
	if _, err := os.Stat(srcDir + "/" + rel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source not removed: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("forgetLowerInode called %d times, want 1: %+v", len(calls), calls)
	}
	// The SOURCE tier's backing path must be forwarded — not its rank. The
	// rank is tierd's numbering; smoothfs indexes tiers by position in the
	// pool's tiers= list, and forwarding a rank silently forgets the wrong
	// tier (see smoothfsTierIndex).
	if calls[0].pool != "pool1" || calls[0].tier != srcDir || calls[0].ino != srcIno {
		t.Fatalf("forgetLowerInode = {pool:%q tier:%q ino:%d}, want {pool1 %q %d}",
			calls[0].pool, calls[0].tier, calls[0].ino, srcDir, srcIno)
	}
}

func TestMoveForPlacementCompletesstalledMoveWhenDestMatchesSrc(t *testing.T) {
	origMountReady := backingMountActive
	backingMountActive = func(string) bool { return true }
	t.Cleanup(func() { backingMountActive = origMountReady })

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	rel := "dir/file.txt"
	mtime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	content := []byte("same content")

	for _, dir := range []string{srcDir + "/dir", dstDir + "/dir"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	for _, p := range []string{srcDir + "/" + rel, dstDir + "/" + rel} {
		if err := writeFile(p, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	a := &Adapter{}
	err := a.moveForPlacement(
		db.MdadmManagedNamespaceRow{PoolName: "pool1", NamespaceID: "ns1"},
		rel,
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "src", MountPath: srcDir},
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "dst", MountPath: dstDir},
		1, 2,
	)
	if err != nil {
		t.Fatalf("moveForPlacement stalled move: %v", err)
	}
	// Source should be gone; destination should still have the content.
	if _, err := os.Stat(srcDir + "/" + rel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("src should have been removed, got: %v", err)
	}
	got, err := readFile(dstDir + "/" + rel)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("dst contents = %q, want %q", got, content)
	}
}

// A byte-identical destination duplicate whose mtime differs from the source
// (a tier copy that did not preserve mtime) must still reconcile — otherwise the
// scanner re-queues the same doomed move forever. Regression for the reconcile
// loop that flooded the log and starved image pulls.
func TestMoveForPlacementReconcilesIdenticalDestWithMtimeSkew(t *testing.T) {
	origMountReady := backingMountActive
	backingMountActive = func(string) bool { return true }
	t.Cleanup(func() { backingMountActive = origMountReady })

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	rel := "dir/file.txt"
	content := []byte("identical bytes across tiers")

	for _, dir := range []string{srcDir + "/dir", dstDir + "/dir"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := writeFile(srcDir+"/"+rel, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := writeFile(dstDir+"/"+rel, content, 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}
	// Same content, deliberately different mtimes.
	if err := os.Chtimes(srcDir+"/"+rel, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("chtimes src: %v", err)
	}
	if err := os.Chtimes(dstDir+"/"+rel, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("chtimes dst: %v", err)
	}

	a := &Adapter{}
	err := a.moveForPlacement(
		db.MdadmManagedNamespaceRow{PoolName: "pool1", NamespaceID: "ns1"},
		rel,
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "src", MountPath: srcDir},
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "dst", MountPath: dstDir},
		1, 2,
	)
	if err != nil {
		t.Fatalf("moveForPlacement should reconcile identical dup with mtime skew, got: %v", err)
	}
	if _, err := os.Stat(srcDir + "/" + rel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale source should have been removed, got: %v", err)
	}
	got, err := readFile(dstDir + "/" + rel)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("dst contents = %q, want %q", got, content)
	}
}

// A same-size but content-DIFFERENT destination is a genuine conflict: it must
// still error and preserve both files (never silently overwrite), even though
// the reconcile path now falls back to a content compare.
func TestMoveForPlacementSameSizeDifferentContentStillErrors(t *testing.T) {
	origMountReady := backingMountActive
	backingMountActive = func(string) bool { return true }
	t.Cleanup(func() { backingMountActive = origMountReady })

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	rel := "dir/file.txt"
	for _, dir := range []string{srcDir + "/dir", dstDir + "/dir"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := writeFile(srcDir+"/"+rel, []byte("AAAA"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := writeFile(dstDir+"/"+rel, []byte("BBBB"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	a := &Adapter{}
	err := a.moveForPlacement(
		db.MdadmManagedNamespaceRow{PoolName: "pool1", NamespaceID: "ns1"},
		rel,
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "src", MountPath: srcDir},
		db.MdadmManagedTargetRow{PoolName: "pool1", TierName: "dst", MountPath: dstDir},
		1, 2,
	)
	if err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("moveForPlacement error = %v, want destination exists", err)
	}
	if got, _ := readFile(srcDir + "/" + rel); string(got) != "AAAA" {
		t.Fatalf("src altered: %q", got)
	}
	if got, _ := readFile(dstDir + "/" + rel); string(got) != "BBBB" {
		t.Fatalf("dst altered: %q", got)
	}
}

func TestInstallPlacementCopyDoesNotOverwriteExistingDestination(t *testing.T) {
	dir := t.TempDir()
	tmp := dir + "/file.tierd-move"
	dst := dir + "/file"
	if err := writeFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := writeFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if err := installPlacementCopy(tmp, dst); err == nil {
		t.Fatal("installPlacementCopy succeeded over an existing destination")
	}
	got, err := readFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("dst contents = %q, want old", got)
	}
}

func TestTierTargetCapBytes(t *testing.T) {
	const tib = int64(1) << 40
	cases := []struct {
		name     string
		total    int64
		fillPct  int
		wantCap  int64
	}{
		{"write-cache tier holds nothing", tib, 0, 0},
		{"half fill", tib, 50, tib / 2},
		{"one percent", 100 << 30, 1, 1 << 30},
		{"full", tib, 100, tib},
		{"negative clamps to zero", tib, -5, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tierTargetCapBytes(c.total, c.fillPct); got != c.wantCap {
				t.Fatalf("tierTargetCapBytes(%d, %d) = %d, want %d", c.total, c.fillPct, got, c.wantCap)
			}
		})
	}
}

func TestTierFullCapBytes(t *testing.T) {
	const tib = int64(1) << 40
	cases := []struct {
		name    string
		total   int64
		fullPct int
		want    int64
	}{
		{"evacuated tier caps at zero", tib, 0, 0},
		{"normal write cap", tib, 95, tib * 95 / 100},
		{"full", tib, 100, tib},
		{"negative clamps to zero", tib, -3, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tierFullCapBytes(c.total, c.fullPct); got != c.want {
				t.Fatalf("tierFullCapBytes(%d, %d) = %d, want %d", c.total, c.fullPct, got, c.want)
			}
		})
	}
}

// TestPlanMoveOrderDrainsBeforeFilling is the regression guard for the .254
// ENOSPC storm: the packer's plan was correct but unreachable in execution
// order. A fast tier sitting over target must first shed the files the plan
// evicted; if a promotion is attempted while those bytes are still resident,
// the copy fails "no space left on device" — and fails again every cycle, since
// the next pass re-plans from the same over-full state (~129k failed HDD→NVME
// copies an hour, tier pinned at 97-100%).
//
// Ordering demotions first is what makes the plan reachable, so this asserts on
// the ORDER of the emitted moves, not merely on their contents.
func TestPlanMoveOrderDrainsBeforeFilling(t *testing.T) {
	cands := []candidate{
		{curRank: 2, size: 10},  // 0: HDD -> NVME  (promotion, consumes space)
		{curRank: 1, size: 500}, // 1: NVME -> HDD  (demotion, frees space)
		{curRank: 1, size: 20},  // 2: already on its assigned rank -> no move
		{curRank: 2, size: 30},  // 3: HDD -> NVME  (promotion)
		{curRank: 1, size: 400}, // 4: NVME -> HDD  (demotion)
	}
	assignments := []int{1, 2, 1, 1, 2}
	assigned := []bool{true, true, true, true, true}

	demotions, promotions := planMoveOrder(cands, assignments, assigned)

	if len(demotions) != 2 {
		t.Fatalf("demotions = %d, want 2", len(demotions))
	}
	if len(promotions) != 2 {
		t.Fatalf("promotions = %d, want 2", len(promotions))
	}
	// The no-op (candidate 2, already on rank 1) must not be planned at all.
	for _, m := range append(demotions, promotions...) {
		if m.idx == 2 {
			t.Fatalf("candidate already on its assigned rank was planned as a move")
		}
	}
	// Every demotion frees fast-tier space; every promotion consumes it.
	for _, m := range demotions {
		if m.want <= cands[m.idx].curRank {
			t.Fatalf("demotion %+v does not move to a slower tier", m)
		}
	}
	for _, m := range promotions {
		if m.want >= cands[m.idx].curRank {
			t.Fatalf("promotion %+v does not move to a faster tier", m)
		}
	}
	// The execution order the caller uses: drains strictly before fills.
	order := append(demotions, promotions...)
	sawPromotion := false
	for _, m := range order {
		isDemotion := m.want > cands[m.idx].curRank
		if !isDemotion {
			sawPromotion = true
			continue
		}
		if sawPromotion {
			t.Fatalf("demotion %+v scheduled after a promotion; fast tier fills before it drains", m)
		}
	}
}

// TestPlanMoveOrderSkipsUnassigned verifies the packer's unassigned candidates
// never become moves.
func TestPlanMoveOrderSkipsUnassigned(t *testing.T) {
	cands := []candidate{
		{curRank: 2, size: 10},
		{curRank: 1, size: 10},
	}
	assignments := []int{1, 2}
	assigned := []bool{false, false}

	demotions, promotions := planMoveOrder(cands, assignments, assigned)
	if len(demotions) != 0 || len(promotions) != 0 {
		t.Fatalf("unassigned candidates produced moves: %d demotions, %d promotions",
			len(demotions), len(promotions))
	}
}

// TestSmoothfsTierIndexMapsPathNotRank pins the boundary that caused the .254
// ENOSPC storm: smoothfs indexes tiers by position in the pool's tiers= list
// (0-based, fastest first), while tierd ranks the same tiers 1..N. Forwarding a
// rank meant rank2 -> tier 2 -> EINVAL (loud, ~331k/hour) and rank1 -> tier 1 ->
// ACCEPTED but aimed at the SECOND tier (silent) — so the fast tier's freed
// blocks were never reclaimed and it could not drain.
func TestSmoothfsTierIndexMapsPathNotRank(t *testing.T) {
	// Pool tier list exactly as stored/mounted: fastest first.
	tiers := []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/HDD"}

	got, ok := smoothfsTierIndex(tiers, "/mnt/.tierd-backing/media/NVME")
	if !ok || got != 0 {
		t.Fatalf("NVME (tierd rank 1) index = %d, ok=%v; want 0 — forwarding the rank would say 1 (HDD)", got, ok)
	}
	got, ok = smoothfsTierIndex(tiers, "/mnt/.tierd-backing/media/HDD")
	if !ok || got != 1 {
		t.Fatalf("HDD (tierd rank 2) index = %d, ok=%v; want 1 — forwarding the rank would say 2 (EINVAL)", got, ok)
	}

	// An unknown path must be reported, never silently coerced to tier 0 —
	// forgetting the wrong tier is how this bug stayed invisible.
	if _, ok := smoothfsTierIndex(tiers, "/mnt/.tierd-backing/media/SSD"); ok {
		t.Fatalf("unknown tier path reported as present")
	}
	if _, ok := smoothfsTierIndex(nil, "/mnt/.tierd-backing/media/NVME"); ok {
		t.Fatalf("empty tier list reported a match")
	}
}
