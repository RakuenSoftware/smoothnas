package mdadm

import (
	"errors"
	"os"
	"strings"
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
	// Fastest tier is at target_fill but below full_threshold. Migration
	// placement spills to the lower tier so the fast tier drains toward fill%.
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: 1 << 29, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	if r := admitWithFallback(caps, ranked, 1, 100<<20); r != 2 {
		t.Errorf("past-target-under-full fastest: got rank %d, want 2", r)
	}
}

func TestAdmitUsesTargetFill(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	usedFast := int64(1<<30) * 60 / 100
	caps := map[int]*tierCapacity{
		1: {
			totalBytes: 1 << 30,
			usedBytes:  usedFast,
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
		t.Errorf("target-fill fastest: got rank %d, want 2", r)
	}

	caps[1].usedBytes = int64(1<<30) * 49 / 100
	if r := admitWithFallback(caps, ranked, 1, 5<<20); r != 1 {
		t.Errorf("target-fill below target: got rank %d, want 1", r)
	}
}

func TestAdmitDrainsWhenOverTargetFill(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
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
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 600, targetCap: 500, fullCap: 950},
		2: {totalBytes: 1000, usedBytes: 500, targetCap: 500, fullCap: 950},
	}

	if r := admitWithFallback(caps, ranked, 1, 100); r != 2 {
		t.Errorf("fallback above target_fill: got rank %d, want 2", r)
	}
	if caps[1].usedBytes != 600 {
		t.Errorf("upper tier usedBytes = %d, want 600", caps[1].usedBytes)
	}
	if caps[2].usedBytes != 600 {
		t.Errorf("bottom tier usedBytes = %d, want 600", caps[2].usedBytes)
	}
}

func TestAssignCandidateRanksDrainsUpperTierTowardTargetFill(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 1000, usedBytes: 500, targetCap: 500, fullCap: 950},
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
		t.Fatalf("candidate 1 assignment = %d, want 2", assignments[1])
	}
	if caps[1].usedBytes != 400 {
		t.Fatalf("upper tier usedBytes = %d, want 400", caps[1].usedBytes)
	}
}

// TestAssignCandidateRanksDrainsLargeFilesFromFastTier ensures that a large
// unpinned file currently on NVME is drained to the size-appropriate slower
// tier even when NVME has capacity below target_fill. Rsync backup writes land
// on NVME first (smoothfs places new files on the fastest tier); without this
// drain, cold backup files permanently occupy fast storage as long as NVME
// is not over its fill target.
func TestAssignCandidateRanksDrainsLargeFilesFromFastTier(t *testing.T) {
	// ranks 1=NVME, 2=HDD. NVME has plenty of room under target_fill.
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 10 << 30, usedBytes: 0, targetCap: 5 << 30, fullCap: (10 << 30) * 95 / 100},
		2: {totalBytes: 100 << 30, usedBytes: 0, targetCap: 50 << 30, fullCap: (100 << 30) * 95 / 100},
	}
	// 1 GB file on NVME. idealRank for 1 GB with 2 tiers = HDD (rank 2).
	cands := []candidate{
		{curRank: 1, size: 1 << 30, pin: meta.PinNone},
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 2 {
		t.Errorf("1 GB file on NVME: assigned rank %d, want 2 (HDD); large backup files must drain from fast tiers", assignments[0])
	}
}

// TestAssignCandidateRanksSmallFilePromotedToIdealTier verifies that a small
// unpinned file on HDD is assigned to its size-ideal tier (NVME) when NVME has
// capacity below target_fill. Size is a bias that sets the starting preference;
// small files prefer fast tiers, large files prefer slow tiers.
func TestAssignCandidateRanksSmallFilePromotedToIdealTier(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 2000, usedBytes: 0, targetCap: 1000, fullCap: 1900},
	}
	// Small file (100 bytes, idealRank=NVME) currently on HDD. NVME has
	// capacity under target_fill, so it should be assigned to NVME.
	cands := []candidate{
		{curRank: 2, size: 100, pin: meta.PinNone},
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 1 {
		t.Errorf("small file on HDD: assigned rank %d, want 1 (NVME); small files bias toward faster tiers", assignments[0])
	}
}

// TestAssignCandidateRanksSmallFileStaysOnHDDWhenFasterTierFull verifies that
// a small file on HDD stays there when the ideal faster tier is full — size
// bias is a preference, capacity decides the actual tier.
func TestAssignCandidateRanksSmallFileStaysOnHDDWhenFasterTierFull(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 960, targetCap: 500, fullCap: 950}, // NVME above full_threshold
		2: {totalBytes: 2000, usedBytes: 0, targetCap: 1000, fullCap: 1900},
	}
	cands := []candidate{
		{curRank: 2, size: 100, pin: meta.PinNone},
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 2 {
		t.Errorf("small file with full NVME: assigned rank %d, want 2 (HDD spill)", assignments[0])
	}
}

// TestAssignCandidateRanksLargeFileOnFullHDDOverflowsToNVME verifies that a
// large file falls back to a faster tier when its preferred slower tiers are
// above full_threshold. Size bias does not strand files when storage is full.
func TestAssignCandidateRanksLargeFileOnFullHDDOverflowsToNVME(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// HDD above full_threshold; NVME has plenty of room.
	caps := map[int]*tierCapacity{
		1: {totalBytes: 10 << 30, usedBytes: 1 << 30, targetCap: 5 << 30, fullCap: (10 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, usedBytes: (10 << 30) * 96 / 100, targetCap: 5 << 30, fullCap: (10 << 30) * 95 / 100},
	}
	cands := []candidate{
		{curRank: 1, size: 1 << 30, pin: meta.PinNone}, // 1 GB, idealRank=HDD
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 1 {
		t.Errorf("1 GB file with full HDD: assigned rank %d, want 1 (NVME overflow)", assignments[0])
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
