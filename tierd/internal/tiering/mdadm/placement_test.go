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

func TestIdealRankPinOverridesHeatAndSize(t *testing.T) {
	// PinHot → fastest regardless of heat or size.
	if got := idealRank(meta.PinHot, 0, 10<<30, 1, 3); got != 1 {
		t.Errorf("PinHot cold 10GB: got %d, want 1", got)
	}
	if got := idealRank(meta.PinHot, 100, 1024, 1, 3); got != 1 {
		t.Errorf("PinHot hot 1KB: got %d, want 1", got)
	}
	// PinCold → slowest regardless of heat or size.
	if got := idealRank(meta.PinCold, 0, 1024, 1, 3); got != 3 {
		t.Errorf("PinCold cold 1KB: got %d, want 3", got)
	}
	if got := idealRank(meta.PinCold, 100, 10<<30, 1, 3); got != 3 {
		t.Errorf("PinCold hot 10GB: got %d, want 3", got)
	}
}

func TestIdealRankHeatIsPrimary(t *testing.T) {
	// heat=0: cold, goes to slowest regardless of size.
	if got := idealRank(meta.PinNone, 0, 1024, 1, 3); got != 3 {
		t.Errorf("cold 1KB: got %d, want 3 (HDD)", got)
	}
	if got := idealRank(meta.PinNone, 0, 1<<30, 1, 3); got != 3 {
		t.Errorf("cold 1GB: got %d, want 3 (HDD)", got)
	}

	// heat=1: one tier above slowest regardless of size.
	// heatBucketRank(1,1,3): steps=0, r=3-1-0=2 (SSD).
	if got := idealRank(meta.PinNone, 1, 1024, 1, 3); got != 2 {
		t.Errorf("warm(1) 1KB: got %d, want 2 (SSD)", got)
	}
	// Large file with heat=1: size nudges one tier slower → HDD.
	if got := idealRank(meta.PinNone, 1, 1<<30, 1, 3); got != 3 {
		t.Errorf("warm(1) 1GB: got %d, want 3 (HDD; size nudge)", got)
	}

	// heat=2: two tiers above slowest (fastestRank for 3-tier).
	// heatBucketRank(2,1,3): steps=1, r=3-1-1=1 (NVME).
	if got := idealRank(meta.PinNone, 2, 1024, 1, 3); got != 1 {
		t.Errorf("hot(2) 1KB: got %d, want 1 (NVME)", got)
	}
	// Large file with heat=2: size nudges one slower → SSD.
	if got := idealRank(meta.PinNone, 2, 1<<30, 1, 3); got != 2 {
		t.Errorf("hot(2) 1GB: got %d, want 2 (SSD; size nudge)", got)
	}
}

func TestHeatBucketRank(t *testing.T) {
	tests := []struct {
		heat    uint32
		fastest int
		slowest int
		want    int
	}{
		{0, 1, 3, 3}, // cold → HDD
		{1, 1, 3, 2}, // any access → SSD
		{2, 1, 3, 1}, // double → NVME
		{3, 1, 3, 1},
		{4, 1, 3, 1},
		{100, 1, 3, 1},
		// 2-tier: any heat → fastest
		{0, 1, 2, 2},
		{1, 1, 2, 1},
		{100, 1, 2, 1},
		// 4-tier
		{0, 1, 4, 4},
		{1, 1, 4, 3},
		{2, 1, 4, 2},
		{4, 1, 4, 1},
		{8, 1, 4, 1},
	}
	for _, tt := range tests {
		got := heatBucketRank(tt.heat, tt.fastest, tt.slowest)
		if got != tt.want {
			t.Errorf("heatBucketRank(%d,%d,%d)=%d, want %d",
				tt.heat, tt.fastest, tt.slowest, got, tt.want)
		}
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

// TestAdmitFillsFastestFirst: empty fastest tier accepts files until it
// reaches full_threshold_pct; files that no longer fit spill to the lower tier.
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

// TestAdmitDoesNotSpillUntilFull: a tier above target_fill but below
// full_threshold still accepts files. target_fill is not a spill threshold —
// full% is. Spilling at target_fill would dump data on the slowest tier while
// the fast tier still had usable headroom.
func TestAdmitDoesNotSpillUntilFull(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// Fastest tier is well past target_fill (50%) but below full (95%).
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1 << 30, usedBytes: (1 << 30) * 60 / 100, targetCap: 1 << 29, fullCap: (1 << 30) * 95 / 100},
		2: {totalBytes: 10 << 30, targetCap: 10 << 29, fullCap: (10 << 30) * 95 / 100},
	}
	// 100 MB fits under full% on the fastest tier (60% + ~10% = 70% < 95%).
	if r := admitWithFallback(caps, ranked, 1, 100<<20); r != 1 {
		t.Errorf("above-target-below-full fastest: got rank %d, want 1 (no spill until full%%)", r)
	}
}

// TestAdmitSpillsOnlyWhenFull: the fastest tier accepts until adding the file
// would exceed full_threshold_pct, then spills to the next tier.
func TestAdmitSpillsOnlyWhenFull(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 900, targetCap: 500, fullCap: 950}, // 100 below full
		2: {totalBytes: 10000, targetCap: 5000, fullCap: 9500},
	}
	// 40 bytes fits under full (900+40=940 <= 950) → stays on fastest.
	if r := admitWithFallback(caps, ranked, 1, 40); r != 1 {
		t.Errorf("fits under full: got rank %d, want 1", r)
	}
	// Now at 940. 50 more would be 990 > 950 full → spills to tier 2.
	if r := admitWithFallback(caps, ranked, 1, 50); r != 2 {
		t.Errorf("would exceed full: got rank %d, want 2", r)
	}
}

// TestAdmitSpillsToSlowerTierWhenFastestFull: a file lands on the next tier
// down when the fastest is at full_threshold, and the slower tier accounts for
// the added bytes.
func TestAdmitSpillsToSlowerTierWhenFastestFull(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 900, targetCap: 500, fullCap: 950}, // only 50 free under full
		2: {totalBytes: 1000, usedBytes: 500, targetCap: 500, fullCap: 950},
	}
	if r := admitWithFallback(caps, ranked, 1, 100); r != 2 {
		t.Errorf("fastest full: got rank %d, want 2", r)
	}
	if caps[1].usedBytes != 900 {
		t.Errorf("upper tier usedBytes = %d, want 900 (untouched)", caps[1].usedBytes)
	}
	if caps[2].usedBytes != 600 {
		t.Errorf("bottom tier usedBytes = %d, want 600", caps[2].usedBytes)
	}
}

// TestAssignCandidateRanksFillsFastTierToFullThenSpills verifies that files
// keep landing on the fast tier until it reaches full_threshold_pct, then
// spill to the slower tier — they do NOT spill at target_fill_pct.
func TestAssignCandidateRanksFillsFastTierToFullThenSpills(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// NVME: total 1000, target_fill 500, full 950. Three 400-byte hot files.
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 10000, usedBytes: 0, targetCap: 5000, fullCap: 9500},
	}
	cands := []candidate{
		{curRank: 1, size: 400, heat: 2, pin: meta.PinNone},
		{curRank: 1, size: 400, heat: 2, pin: meta.PinNone},
		{curRank: 1, size: 400, heat: 2, pin: meta.PinNone},
	}

	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	for i := range cands {
		if !assigned[i] {
			t.Fatalf("candidate %d was not assigned", i)
		}
	}
	// 1st: 0→400 (≤950) NVME. 2nd: 400→800 (≤950) NVME — past target_fill 500
	// but below full, so it stays. 3rd: 800→1200 (>950) spills to HDD.
	if assignments[0] != 1 || assignments[1] != 1 {
		t.Fatalf("first two assignments = %d,%d, want 1,1 (fill to full%%, not target)", assignments[0], assignments[1])
	}
	if assignments[2] != 2 {
		t.Fatalf("third assignment = %d, want 2 (NVME at full, spills)", assignments[2])
	}
	if caps[1].usedBytes != 800 {
		t.Fatalf("upper tier usedBytes = %d, want 800", caps[1].usedBytes)
	}
}

// TestAssignCandidateRanksColdFilesFillNVMEBeforeSpillingToHDD verifies that
// cold files use available NVME capacity (fill-before-spill) rather than
// bypassing NVME and going directly to HDD when NVME has headroom.
func TestAssignCandidateRanksColdFilesFillNVMEBeforeSpillingToHDD(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
	}
	cands := []candidate{
		{curRank: 1, size: 100, heat: 0, pin: meta.PinNone}, // cold on NVME, NVME has room
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	// Cold file uses NVME capacity (fill-before-spill). It only drains to HDD
	// once NVME is full.
	if assignments[0] != 1 {
		t.Errorf("cold file with NVME headroom: assigned rank %d, want 1 (NVME)", assignments[0])
	}
}

// TestAssignCandidateRanksColdFilesSpillToHDDWhenNVMEFull verifies that cold
// files drain to HDD once NVME is at full_threshold_pct (not target_fill).
func TestAssignCandidateRanksColdFilesSpillToHDDWhenNVMEFull(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 950, targetCap: 500, fullCap: 950}, // NVME at full
		2: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
	}
	cands := []candidate{
		{curRank: 1, size: 100, heat: 0, pin: meta.PinNone},
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 2 {
		t.Errorf("cold file with full NVME: assigned rank %d, want 2 (HDD)", assignments[0])
	}
}

// TestAssignCandidateRanksColdFilesUseNVMEWhenAvailable verifies that a cold
// large file stays on NVME while NVME has headroom. Backups land on NVME first
// (smoothfs write path); the planner leaves them there until NVME fills up.
func TestAssignCandidateRanksColdFilesUseNVMEWhenAvailable(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 10 << 30, usedBytes: 0, targetCap: 5 << 30, fullCap: (10 << 30) * 95 / 100},
		2: {totalBytes: 100 << 30, usedBytes: 0, targetCap: 50 << 30, fullCap: (100 << 30) * 95 / 100},
	}
	cands := []candidate{
		{curRank: 1, size: 1 << 30, heat: 0, pin: meta.PinNone}, // cold 1 GB on NVME
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 1 {
		t.Errorf("cold large file with NVME headroom: assigned rank %d, want 1 (NVME; fill-before-spill)", assignments[0])
	}
}

// TestAssignCandidateRanksHotFilesDisplaceColdFilesToHDD verifies that hot
// files displace cold files from NVME: hot files fill NVME first (sorted
// ahead of cold), and once NVME reaches full_threshold the remaining cold
// file spills to HDD.
func TestAssignCandidateRanksHotFilesDisplaceColdFilesToHDD(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	// NVME holds up to full=950; HDD has plenty of room.
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 10000, usedBytes: 0, targetCap: 5000, fullCap: 9500},
	}
	// Hot file (sorted first) takes 950 bytes, filling NVME to full.
	// Cold file (sorted last) finds NVME at full → spills to HDD.
	cands := []candidate{
		{curRank: 1, size: 950, heat: 4, pin: meta.PinNone}, // hot, fills NVME to full
		{curRank: 1, size: 100, heat: 0, pin: meta.PinNone}, // cold, arrives after NVME is full
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	for i := range cands {
		if !assigned[i] {
			t.Fatalf("candidate %d was not assigned", i)
		}
	}
	if assignments[0] != 1 {
		t.Errorf("hot file: assigned rank %d, want 1 (NVME)", assignments[0])
	}
	if assignments[1] != 2 {
		t.Errorf("cold file: assigned rank %d, want 2 (HDD; displaced by hot file)", assignments[1])
	}
}

// TestAssignCandidateRanksHotSmallFilePromotedToNVME verifies that a warm/hot
// small file on HDD is promoted to NVME. Heat drives promotion; size confirms
// the fast tier is appropriate for small files.
func TestAssignCandidateRanksHotSmallFilePromotedToNVME(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 2000, usedBytes: 0, targetCap: 1000, fullCap: 1900},
	}
	// heat=2 → heatBucketRank=1 (NVME for 2-tier); small size → no size nudge.
	cands := []candidate{
		{curRank: 2, size: 100, heat: 2, pin: meta.PinNone},
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 1 {
		t.Errorf("hot small file on HDD: assigned rank %d, want 1 (NVME)", assignments[0])
	}
}

// TestAssignCandidateRanksColdSmallFileMovesToNVMEWhenAvailable verifies that
// a cold small file on HDD is assigned to NVME when NVME has headroom.
// Fill-before-spill: all tiers above HDD should be utilized before data
// accumulates on HDD.
func TestAssignCandidateRanksColdSmallFileMovesToNVMEWhenAvailable(t *testing.T) {
	ranked := testRanked(testRanking{1, 50, 95}, testRanking{2, 50, 95})
	caps := map[int]*tierCapacity{
		1: {totalBytes: 1000, usedBytes: 0, targetCap: 500, fullCap: 950},
		2: {totalBytes: 2000, usedBytes: 0, targetCap: 1000, fullCap: 1900},
	}
	cands := []candidate{
		{curRank: 2, size: 100, heat: 0, pin: meta.PinNone},
	}
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	if !assigned[0] {
		t.Fatal("candidate was not assigned")
	}
	if assignments[0] != 1 {
		t.Errorf("cold small file: assigned rank %d, want 1 (NVME has headroom)", assignments[0])
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
	// Same inode on two tiers (duplicate): hot on NVME, cold on HDD.
	// Each is placed independently by heat+size — not by curRank.
	cands := []candidate{
		{inode: 42, curRank: 1, size: 400, heat: 2, pin: meta.PinNone}, // hot → NVME
		{inode: 42, curRank: 2, size: 700, heat: 0, pin: meta.PinNone}, // cold → HDD
	}

	assignments, assigned := assignCandidateRanks(cands, caps, ranked, 1, 2)
	for i := range cands {
		if !assigned[i] {
			t.Fatalf("candidate %d was not assigned", i)
		}
	}
	if assignments[0] != 1 {
		t.Fatalf("candidate 0 (hot) assignment = %d, want 1 (NVME)", assignments[0])
	}
	if assignments[1] != 2 {
		t.Fatalf("candidate 1 (cold) assignment = %d, want 2 (HDD)", assignments[1])
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
