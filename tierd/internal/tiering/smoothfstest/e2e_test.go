//go:build linux && e2e

package smoothfstest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	smoothfs "github.com/RakuenSoftware/smoothfs"
	controlplane "github.com/RakuenSoftware/smoothfs/controlplane"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

type (
	Service       = controlplane.Service
	Pool          = controlplane.Pool
	MovementPlan  = controlplane.MovementPlan
	Client        = smoothfs.Client
	InspectResult = smoothfs.InspectResult
	PinState      = smoothfs.PinState
	MovementState = smoothfs.MovementState
)

const OIDLen = smoothfs.OIDLen

const (
	StatePlaced   = smoothfs.StatePlaced
	StateSwitched = smoothfs.StateSwitched
	PinNone       = smoothfs.PinNone
	PinLease      = smoothfs.PinLease
)

var (
	NewService = controlplane.NewService
	NewWorker  = controlplane.NewWorker
	Open       = smoothfs.Open
)

type e2eEnv struct {
	t *testing.T

	root       string
	sqlDB      *sql.DB
	poolUUID   uuid.UUID
	fastMount  string
	slowMount  string
	mountpoint string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	svc    *Service
}

type lowerFSSpec struct {
	name      string
	mkfsTool  string
	mkfsArgs  []string
	mountType string
	imageSize string
}

var (
	lowerFSXFS = lowerFSSpec{
		name:      "xfs",
		mkfsTool:  "mkfs.xfs",
		mkfsArgs:  []string{"-q"},
		mountType: "",
		imageSize: "512M",
	}
	lowerFSEXT4 = lowerFSSpec{
		name:      "ext4",
		mkfsTool:  "mkfs.ext4",
		mkfsArgs:  []string{"-F"},
		mountType: "",
		imageSize: "512M",
	}
	lowerFSBtrfs = lowerFSSpec{
		name:      "btrfs",
		mkfsTool:  "mkfs.btrfs",
		mkfsArgs:  []string{"-f"},
		mountType: "btrfs",
		imageSize: "1G",
	}
)

func TestE2EMountReadyAutoDiscovery(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{plannerIntervalSec: 1})
	pool := env.waitPool(10 * time.Second)
	if pool == nil {
		t.Fatalf("mount-ready auto-discovery did not register pool %s", env.poolUUID)
	}
}

func TestE2ECompatibilityMatrix(t *testing.T) {
	for _, fs := range []lowerFSSpec{lowerFSXFS, lowerFSEXT4, lowerFSBtrfs} {
		t.Run(fs.name, func(t *testing.T) {
			env := newE2EEnvWithFS(t, e2eConfig{plannerIntervalSec: 3600}, fs, nil)
			_ = env.waitPool(10 * time.Second)

			relPath := filepath.Join("nested", "dir", "hello.txt")
			if err := os.MkdirAll(filepath.Join(env.mountpoint, "nested", "dir"), 0o755); err != nil {
				t.Fatalf("mkdir through smoothfs mount: %v", err)
			}
			payload := []byte(fs.name + "-compat\n")
			if err := os.WriteFile(filepath.Join(env.mountpoint, relPath), payload, 0o644); err != nil {
				t.Fatalf("write through smoothfs mount: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(env.mountpoint, relPath))
			if err != nil {
				t.Fatalf("read through smoothfs mount: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("mount content = %q, want %q", got, payload)
			}
			if _, err := os.Stat(filepath.Join(env.mountpoint, relPath)); err != nil {
				t.Fatalf("stat through smoothfs mount: %v", err)
			}
			if _, err := readOIDXattr(filepath.Join(env.fastMount, relPath)); err != nil {
				t.Fatalf("read lower oid xattr: %v", err)
			}

			renamed := filepath.Join("nested", "dir", "renamed.txt")
			if err := os.Rename(filepath.Join(env.mountpoint, relPath), filepath.Join(env.mountpoint, renamed)); err != nil {
				t.Fatalf("rename through smoothfs mount: %v", err)
			}
			if _, err := os.Stat(filepath.Join(env.fastMount, renamed)); err != nil {
				t.Fatalf("renamed lower file missing: %v", err)
			}
			if err := os.Remove(filepath.Join(env.mountpoint, renamed)); err != nil {
				t.Fatalf("unlink through smoothfs mount: %v", err)
			}
			if _, err := os.Stat(filepath.Join(env.fastMount, renamed)); !os.IsNotExist(err) {
				t.Fatalf("lower file still present after unlink: %v", err)
			}
		})
	}
}

func TestE2EBtrfsReflinkAndSubvolume(t *testing.T) {
	env := newE2EEnvWithFS(t, e2eConfig{plannerIntervalSec: 3600}, lowerFSBtrfs, func(t *testing.T, env *e2eEnv) {
		run(t, "", "btrfs", "subvolume", "create", filepath.Join(env.fastMount, "subvol-fast"))
		run(t, "", "btrfs", "subvolume", "create", filepath.Join(env.slowMount, "subvol-fast"))
	})
	_ = env.waitPool(10 * time.Second)

	srcPath := filepath.Join(env.mountpoint, "subvol-fast", "source.txt")
	clonePath := filepath.Join(env.mountpoint, "subvol-fast", "clone.txt")
	if err := os.WriteFile(srcPath, []byte("btrfs-reflink\n"), 0o644); err != nil {
		t.Fatalf("write reflink source: %v", err)
	}
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("open reflink source: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(clonePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open reflink dest: %v", err)
	}
	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err != nil {
		dst.Close()
		t.Fatalf("FICLONE through smoothfs mount: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close reflink dest: %v", err)
	}
	got, err := os.ReadFile(clonePath)
	if err != nil {
		t.Fatalf("read reflink clone: %v", err)
	}
	if string(got) != "btrfs-reflink\n" {
		t.Fatalf("clone content = %q, want %q", got, "btrfs-reflink\n")
	}
	if _, err := readOIDXattr(filepath.Join(env.fastMount, "subvol-fast", "clone.txt")); err != nil {
		t.Fatalf("read clone oid xattr: %v", err)
	}
}

func TestE2EPhase3NonFunctionalTargets(t *testing.T) {
	if os.Getenv("SMOOTHFS_PERF") == "" {
		t.Skip("set SMOOTHFS_PERF=1 to run the Phase 3 non-functional target suite")
	}

	env := newE2EEnv(t, e2eConfig{plannerIntervalSec: 3600})
	_ = env.waitPool(10 * time.Second)
	env.stopService()

	nativeDir := filepath.Join(env.fastMount, "perf-native")
	smoothDir := filepath.Join(env.mountpoint, "perf-smooth")
	for _, dir := range []string{nativeDir, smoothDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	createPayload := bytes.Repeat([]byte("c"), 200*1024)
	stageStart := time.Now()
	t.Logf("Phase 3 perf: starting native create corpus")
	nativeCreate := benchmarkCreateLatency(t, filepath.Join(nativeDir, "create"), createPayload, 5, 50)
	t.Logf("Phase 3 perf: finished native create corpus in %s", time.Since(stageStart))
	stageStart = time.Now()
	t.Logf("Phase 3 perf: starting smooth create corpus")
	smoothCreate := benchmarkCreateLatency(t, filepath.Join(smoothDir, "create"), createPayload, 5, 50)
	t.Logf("Phase 3 perf: finished smooth create corpus in %s", time.Since(stageStart))
	stageStart = time.Now()
	t.Logf("Phase 3 perf: cleaning native create corpus")
	if err := os.RemoveAll(filepath.Join(nativeDir, "create")); err != nil {
		t.Fatalf("cleanup native create corpus: %v", err)
	}
	t.Logf("Phase 3 perf: finished cleaning native create corpus in %s", time.Since(stageStart))
	stageStart = time.Now()
	t.Logf("Phase 3 perf: cleaning smooth create corpus")
	if err := os.RemoveAll(filepath.Join(smoothDir, "create")); err != nil {
		t.Fatalf("cleanup smooth create corpus: %v", err)
	}
	t.Logf("Phase 3 perf: finished cleaning smooth create corpus in %s", time.Since(stageStart))

	stageStart = time.Now()
	t.Logf("Phase 3 perf: starting native sequential write")
	nativeWrite := benchmarkSequentialWrite(t, filepath.Join(nativeDir, "seq-write.bin"), 64<<20)
	t.Logf("Phase 3 perf: finished native sequential write in %s", time.Since(stageStart))
	stageStart = time.Now()
	t.Logf("Phase 3 perf: starting smooth sequential write")
	smoothWrite := benchmarkSequentialWrite(t, filepath.Join(smoothDir, "seq-write.bin"), 64<<20)
	t.Logf("Phase 3 perf: finished smooth sequential write in %s", time.Since(stageStart))
	stageStart = time.Now()
	t.Logf("Phase 3 perf: starting native sequential read")
	nativeRead := benchmarkSequentialRead(t, nativeWrite.Path)
	t.Logf("Phase 3 perf: finished native sequential read in %s", time.Since(stageStart))
	stageStart = time.Now()
	t.Logf("Phase 3 perf: starting smooth sequential read")
	smoothRead := benchmarkSequentialRead(t, smoothWrite.Path)
	t.Logf("Phase 3 perf: finished smooth sequential read in %s", time.Since(stageStart))

	stageStart = time.Now()
	t.Logf("Phase 3 perf: starting native metadata stat loop")
	nativeMeta := benchmarkStatLatency(t, filepath.Join(nativeDir, "meta.txt"), 2000)
	t.Logf("Phase 3 perf: finished native metadata stat loop in %s", time.Since(stageStart))
	stageStart = time.Now()
	t.Logf("Phase 3 perf: starting smooth metadata stat loop")
	smoothMeta := benchmarkStatLatency(t, filepath.Join(smoothDir, "meta.txt"), 2000)
	t.Logf("Phase 3 perf: finished smooth metadata stat loop in %s", time.Since(stageStart))

	createRatio := ratio(smoothCreate.P99.Seconds(), nativeCreate.P99.Seconds())
	readRatio := ratio(smoothRead.ThroughputMBps, nativeRead.ThroughputMBps)
	writeRatio := ratio(smoothWrite.ThroughputMBps, nativeWrite.ThroughputMBps)
	metaRatio := ratio(smoothMeta.P99.Seconds(), nativeMeta.P99.Seconds())
	cpuRatio := ratio(smoothWrite.CPUSeconds+smoothRead.CPUSeconds, nativeWrite.CPUSeconds+nativeRead.CPUSeconds)

	t.Logf("Phase 3 perf summary: create p99 native=%s smooth=%s ratio=%.2fx; read native=%.1fMB/s smooth=%.1fMB/s ratio=%.2f; write native=%.1fMB/s smooth=%.1fMB/s ratio=%.2f; stat p99 native=%s smooth=%s ratio=%.2fx; cpu native=%.3fs smooth=%.3fs ratio=%.2f",
		nativeCreate.P99, smoothCreate.P99, createRatio,
		nativeRead.ThroughputMBps, smoothRead.ThroughputMBps, readRatio,
		nativeWrite.ThroughputMBps, smoothWrite.ThroughputMBps, writeRatio,
		nativeMeta.P99, smoothMeta.P99, metaRatio,
		nativeWrite.CPUSeconds+nativeRead.CPUSeconds,
		smoothWrite.CPUSeconds+smoothRead.CPUSeconds, cpuRatio)

	if createRatio > 2.0 {
		t.Fatalf("CREATE p99 ratio %.2fx exceeds 2.0x target", createRatio)
	}
	if readRatio < 0.95 {
		t.Fatalf("read throughput ratio %.2f is below 0.95 target", readRatio)
	}
	if writeRatio < 0.90 {
		t.Fatalf("write throughput ratio %.2f is below 0.90 target", writeRatio)
	}
	if metaRatio > 1.5 {
		t.Fatalf("metadata p99 ratio %.2fx exceeds 1.5x target", metaRatio)
	}
	if cpuRatio > 1.10 {
		t.Fatalf("CPU ratio %.2f exceeds 1.10 target", cpuRatio)
	}
}

func TestE2EHeatFlowsIntoPlanner(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{
		plannerIntervalSec: 1,
		minResidencySec:    0,
		cooldownSec:        0,
	})
	ctl := openE2EClient(t)
	pool := env.waitPool(10 * time.Second)

	relPath := "heat.txt"
	if err := os.WriteFile(filepath.Join(env.mountpoint, relPath), []byte("phase-2.6\n"), 0o644); err != nil {
		t.Fatalf("write through smoothfs mount: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(env.mountpoint, relPath)); err != nil {
		t.Fatalf("read through smoothfs mount: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(env.mountpoint, relPath), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("hot\n"); err != nil {
		f.Close()
		t.Fatalf("append through smoothfs mount: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close append fd: %v", err)
	}

	oid := env.seedMountedObject(relPath, "tier-fast")
	if err := ctl.Reconcile(env.poolUUID, "e2e-heat"); err != nil {
		t.Fatalf("kick heat drain: %v", err)
	}

	ewma := env.waitEWMA(oid, 10*time.Second)
	if ewma <= 0 {
		t.Fatalf("ewma = %v, want > 0", ewma)
	}
	env.waitCurrentTier(oid, "tier-slow", 15*time.Second)

	if _, err := os.Stat(filepath.Join(env.slowMount, relPath)); err != nil {
		t.Fatalf("moved file missing from slow tier: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.fastMount, relPath)); !os.IsNotExist(err) {
		t.Fatalf("source file still present on fast tier after move: %v", err)
	}
	if pool == nil {
		t.Fatalf("pool registration lost during heat flow test")
	}
}

func TestE2ERestartReplayPreCutoverRollback(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{
		plannerIntervalSec: 3600,
		minResidencySec:    0,
		cooldownSec:        0,
	})
	ctl := openE2EClient(t)
	pool := env.waitPool(10 * time.Second)

	relPath := "rollback.txt"
	payload := []byte("replay-pre-cutover\n")
	if err := os.WriteFile(filepath.Join(env.mountpoint, relPath), payload, 0o644); err != nil {
		t.Fatalf("write through smoothfs mount: %v", err)
	}
	oid := env.seedMountedObject(relPath, "tier-fast")
	if err := ctl.MovePlan(env.poolUUID, oid, 1, 7); err != nil {
		t.Fatalf("move_plan: %v", err)
	}
	execSQL(t, env.sqlDB, `
		UPDATE smoothfs_objects
		   SET intended_tier_id = ?,
		       movement_state = 'plan_accepted',
		       transaction_seq = 7,
		       updated_at = datetime('now')
		 WHERE object_id = ?`, "tier-slow", hex.EncodeToString(oid[:]))

	env.stopService()
	env.unmount()
	env.startService()
	env.mount()
	pool = env.waitPool(10 * time.Second)

	ins := env.waitInspectStateWithClient(ctl, oid, StatePlaced, 0, 10*time.Second)
	if ins.IntendedTier != 0 {
		t.Fatalf("intended_tier after replay = %d, want 0", ins.IntendedTier)
	}
	env.waitDBPlacedState(oid, "tier-fast", 10*time.Second)
	got, err := os.ReadFile(filepath.Join(env.mountpoint, relPath))
	if err != nil {
		t.Fatalf("read replayed file through mount: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("mount content = %q, want %q", got, payload)
	}

	plan := MovementPlan{
		PoolUUID:       env.poolUUID,
		ObjectID:       oid,
		NamespaceID:    pool.NamespaceID,
		SourceTierID:   "tier-fast",
		SourceTierRank: 0,
		SourceLowerDir: env.fastMount,
		DestTierID:     "tier-slow",
		DestTierRank:   1,
		DestLowerDir:   env.slowMount,
		RelPath:        relPath,
		TransactionSeq: pool.NextSeq(),
	}
	workerClient := openE2EClient(t)
	if err := NewWorker(env.sqlDB, workerClient).Execute(env.ctx, plan); err != nil {
		t.Fatalf("retry execute: %v", err)
	}

	env.waitCurrentTier(oid, "tier-slow", 10*time.Second)
	got, err = os.ReadFile(filepath.Join(env.slowMount, relPath))
	if err != nil {
		t.Fatalf("read moved file from slow tier: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("slow-tier content = %q, want %q", got, payload)
	}
}

func TestE2ERestartReplayPostCutoverForward(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{
		plannerIntervalSec: 3600,
		minResidencySec:    0,
		cooldownSec:        0,
	})
	ctl := openE2EClient(t)
	_ = env.waitPool(10 * time.Second)

	relPath := "forward.txt"
	payload := []byte("replay-post-cutover\n")
	if err := os.WriteFile(filepath.Join(env.mountpoint, relPath), payload, 0o644); err != nil {
		t.Fatalf("write through smoothfs mount: %v", err)
	}
	oid := env.seedMountedObject(relPath, "tier-fast")
	if err := ctl.MovePlan(env.poolUUID, oid, 1, 11); err != nil {
		t.Fatalf("move_plan: %v", err)
	}

	dstPath := filepath.Join(env.slowMount, relPath)
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		t.Fatalf("mkdir slow-tier parent: %v", err)
	}
	srcBytes, err := os.ReadFile(filepath.Join(env.fastMount, relPath))
	if err != nil {
		t.Fatalf("read fast-tier source: %v", err)
	}
	if err := os.WriteFile(dstPath, srcBytes, 0o644); err != nil {
		t.Fatalf("write slow-tier staged copy: %v", err)
	}
	if err := unix.Setxattr(dstPath, "trusted.smoothfs.oid", oid[:], 0); err != nil {
		t.Fatalf("set slow-tier oid xattr: %v", err)
	}
	if err := ctl.MoveCutover(env.poolUUID, oid, 11); err != nil {
		t.Fatalf("move_cutover: %v", err)
	}
	execSQL(t, env.sqlDB, `
		UPDATE smoothfs_objects
		   SET intended_tier_id = ?,
		       movement_state = 'cleanup_in_progress',
		       transaction_seq = 11,
		       updated_at = datetime('now')
		 WHERE object_id = ?`, "tier-slow", hex.EncodeToString(oid[:]))

	env.stopService()
	env.unmount()
	env.startService()
	env.mount()
	env.waitPool(10 * time.Second)

	ins := env.waitInspectStateWithClient(ctl, oid, StatePlaced, 1, 10*time.Second)
	if ins.IntendedTier != 1 {
		t.Fatalf("intended_tier after forward replay = %d, want 1", ins.IntendedTier)
	}
	env.waitDBPlacedState(oid, "tier-slow", 10*time.Second)
	got, err := os.ReadFile(filepath.Join(env.mountpoint, relPath))
	if err != nil {
		t.Fatalf("read replay-forward file through mount: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("mount content = %q, want %q", got, payload)
	}
	fastGot, err := os.ReadFile(filepath.Join(env.fastMount, relPath))
	if err != nil {
		t.Fatalf("read stale fast-tier copy: %v", err)
	}
	if string(fastGot) != string(payload) {
		t.Fatalf("fast-tier stale content = %q, want %q", fastGot, payload)
	}
}

// TestE2ENFSBasicMount — Phase 4.1 gate: client mounts the smoothfs
// pool via NFSv4.2 over loopback, reads a file written server-side,
// writes back, verifies the write is visible at the smoothfs
// mountpoint. Skipped if nfs-kernel-server isn't available on the
// host. Uses a fixed pool UUID so the export's fsid= is deterministic
// (matches what /etc/exports would contain in production).
func TestE2ENFSBasicMount(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{plannerIntervalSec: 3600})
	_ = env.waitPool(10 * time.Second)

	// Write a probe through the smoothfs mount before we hand it to nfsd.
	const probe = "hello-from-smoothfs\n"
	probePath := filepath.Join(env.mountpoint, "nfs-probe.txt")
	if err := os.WriteFile(probePath, []byte(probe), 0o644); err != nil {
		t.Fatalf("seed probe via smoothfs mount: %v", err)
	}
	// sync(2) fans out to every sb's sync_fs, including smoothfs's
	// (drains the deferred OID xattr writeback queue) and the lower
	// XFS (commits the data pages). Without this, nfsd can race the
	// write through the page cache.
	unix.Sync()

	clientMnt := nfsExportSetup(t, env, "01234567-89ab-cdef-0123-456789abcdef")

	// Read the probe back through NFS. Cross-check against the direct
	// smoothfs read on failure — splits "smoothfs broken" from
	// "smoothfs/nfsd interaction broken".
	got, err := os.ReadFile(filepath.Join(clientMnt, "nfs-probe.txt"))
	if err != nil {
		t.Fatalf("read probe via NFS: %v", err)
	}
	if string(got) != probe {
		direct, _ := os.ReadFile(probePath)
		t.Fatalf("NFS read = %q, want %q (direct smoothfs read = %q)", got, probe, direct)
	}

	// Write back through NFS, verify visible at the smoothfs mount.
	const reply = "from-nfs-client\n"
	if err := os.WriteFile(filepath.Join(clientMnt, "nfs-reply.txt"), []byte(reply), 0o644); err != nil {
		t.Fatalf("write via NFS: %v", err)
	}
	mirror, err := os.ReadFile(filepath.Join(env.mountpoint, "nfs-reply.txt"))
	if err != nil {
		t.Fatalf("read NFS-write back via smoothfs mount: %v", err)
	}
	if string(mirror) != reply {
		t.Fatalf("server-side mirror = %q, want %q", mirror, reply)
	}
}

// nfsExportSetup writes /etc/exports for one smoothfs pool, runs
// exportfs -ra, mounts NFSv4.2 loopback at clientMnt, and registers
// cleanups in t.Cleanup so the export and the lazy-detach happen
// before env's chained smoothfs umount runs (which is non-lazy and
// would EBUSY against nfsd's filecache hold).
func nfsExportSetup(t *testing.T, env *e2eEnv, exportUUID string) (clientMnt string) {
	t.Helper()
	for _, tool := range []string{"exportfs", "mount.nfs", "systemctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing %s — install nfs-kernel-server / nfs-common", tool)
		}
	}
	if exec.Command("systemctl", "is-active", "--quiet", "nfs-kernel-server").Run() != nil {
		t.Skip("nfs-kernel-server not active — `systemctl start nfs-kernel-server`")
	}

	const sentinel = "# tierd-e2e-nfs-test\n"
	exportLine := fmt.Sprintf("%s %s 127.0.0.1(rw,sync,no_root_squash,no_subtree_check,fsid=%s)\n",
		sentinel, env.mountpoint, exportUUID)
	exportsBefore, _ := os.ReadFile("/etc/exports")
	if err := os.WriteFile("/etc/exports", append(exportsBefore, []byte(exportLine)...), 0o644); err != nil {
		t.Fatalf("write /etc/exports: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("exportfs", "-u", "127.0.0.1:"+env.mountpoint).Run()
		_ = os.WriteFile("/etc/exports", exportsBefore, 0o644)
		_ = exec.Command("exportfs", "-ra").Run()
		_ = exec.Command("umount", "-l", env.mountpoint).Run()
	})
	if out, err := exec.Command("exportfs", "-ra").CombinedOutput(); err != nil {
		t.Fatalf("exportfs -ra: %s: %v", strings.TrimSpace(string(out)), err)
	}

	clientMnt = filepath.Join(env.root, "nfs-client")
	if err := os.MkdirAll(clientMnt, 0o755); err != nil {
		t.Fatalf("mkdir client: %v", err)
	}
	if out, err := exec.Command("mount", "-t", "nfs", "-o", "vers=4.2",
		"127.0.0.1:"+env.mountpoint, clientMnt).CombinedOutput(); err != nil {
		t.Fatalf("mount nfs: %s: %v", strings.TrimSpace(string(out)), err)
	}
	t.Cleanup(func() {
		_ = exec.Command("umount", "-l", clientMnt).Run()
	})
	return clientMnt
}

// TestE2ENFSMovementAcrossOpenFD — Phase 4.2 gate. The hardest
// sub-problem in the Phase 4 plan: a long-lived NFS-side fd survives
// a MOVE_PLAN + MOVE_CUTOVER underneath, with reads continuing to
// return the right bytes and writes landing on the new lower tier.
//
// Strategy: drive the movement state machine directly via netlink
// (skipping the worker), the same pattern TestE2ERestartReplay tests
// use. The kernel's per-fd reissue protocol (smoothfs_lower_file
// re-resolves against si->lower_path when cutover_gen advances) is
// what makes this work — this test is the proof.
func TestE2ENFSMovementAcrossOpenFD(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{
		plannerIntervalSec: 3600, // planner effectively off
		minResidencySec:    0,
		cooldownSec:        0,
	})
	ctl := openE2EClient(t)
	_ = env.waitPool(10 * time.Second)

	// Seed the file via the smoothfs mount, register it in SQLite.
	const relPath = "movement-probe.txt"
	initial := []byte("phase4.2-initial-content\n")
	probePath := filepath.Join(env.mountpoint, relPath)
	if err := os.WriteFile(probePath, initial, 0o644); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	unix.Sync()
	oid := env.seedMountedObject(relPath, "tier-fast")

	// Stand up NFSv4.2 export + client mount.
	clientMnt := nfsExportSetup(t, env, "11111111-2222-3333-4444-555555555555")
	nfsPath := filepath.Join(clientMnt, relPath)

	// Open fd via NFS for read+write. Hold it across the cutover.
	nfsFile, err := os.OpenFile(nfsPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("NFS open: %v", err)
	}
	t.Cleanup(func() { nfsFile.Close() })

	// Pre-cutover read sanity.
	got := make([]byte, len(initial))
	if _, err := io.ReadFull(nfsFile, got); err != nil {
		t.Fatalf("pre-cutover NFS read: %v", err)
	}
	if !bytes.Equal(got, initial) {
		t.Fatalf("pre-cutover read = %q, want %q", got, initial)
	}

	// Drive movement directly: MOVE_PLAN to slow, stage destination
	// (tierd worker would do this), MOVE_CUTOVER. Transaction seq is
	// arbitrary as long as it's monotonic.
	if err := ctl.MovePlan(env.poolUUID, oid, 1, 100); err != nil {
		t.Fatalf("MOVE_PLAN: %v", err)
	}
	dstPath := filepath.Join(env.slowMount, relPath)
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		t.Fatalf("mkdir slow parent: %v", err)
	}
	if err := os.WriteFile(dstPath, initial, 0o644); err != nil {
		t.Fatalf("stage slow copy: %v", err)
	}
	if err := unix.Setxattr(dstPath, "trusted.smoothfs.oid", oid[:], 0); err != nil {
		t.Fatalf("set slow oid xattr: %v", err)
	}
	if err := ctl.MoveCutover(env.poolUUID, oid, 100); err != nil {
		t.Fatalf("MOVE_CUTOVER: %v", err)
	}

	// Post-cutover read on the SAME open NFS fd. The smoothfs side
	// reissues lower_file against the new lower_path on the next I/O.
	if _, err := nfsFile.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("post-cutover seek: %v", err)
	}
	if _, err := io.ReadFull(nfsFile, got); err != nil {
		t.Fatalf("post-cutover NFS read: %v", err)
	}
	if !bytes.Equal(got, initial) {
		t.Fatalf("post-cutover read = %q, want %q", got, initial)
	}

	// Post-cutover write on the SAME open NFS fd. Should land on the
	// slow tier (the new authoritative lower).
	appended := []byte("APPEND-AFTER-CUTOVER\n")
	if _, err := nfsFile.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("seek end: %v", err)
	}
	if _, err := nfsFile.Write(appended); err != nil {
		t.Fatalf("post-cutover NFS write: %v", err)
	}
	if err := nfsFile.Sync(); err != nil {
		t.Fatalf("nfs fsync: %v", err)
	}
	unix.Sync()

	// Verify final state at all three observation points.
	want := append(append([]byte{}, initial...), appended...)
	if slowGot, err := os.ReadFile(dstPath); err != nil {
		t.Fatalf("read final from slow tier: %v", err)
	} else if !bytes.Equal(slowGot, want) {
		t.Fatalf("slow tier final = %q, want %q", slowGot, want)
	}
	if smoothGot, err := os.ReadFile(probePath); err != nil {
		t.Fatalf("read final via smoothfs mount: %v", err)
	} else if !bytes.Equal(smoothGot, want) {
		t.Fatalf("smoothfs mount final = %q, want %q", smoothGot, want)
	}
	// And via NFS — `os.Stat` on a fresh path goes through the
	// NFS LOOKUP / GETATTR path, exercising attribute propagation
	// post-cutover (the agent's stat-after-cutover concern). If the
	// upper inode held stale source-tier attrs we'd see the
	// pre-cutover size here.
	if st, err := os.Stat(nfsPath); err != nil {
		t.Fatalf("post-cutover NFS stat: %v", err)
	} else if st.Size() != int64(len(want)) {
		t.Fatalf("post-cutover NFS stat size = %d, want %d", st.Size(), len(want))
	}
}

// TestE2ENFSReconnectAcrossServerUmount — Phase 4.3 gate #1. Client
// has an open fd + cached handle to a file on a smoothfs NFS export.
// Server umounts+remounts smoothfs (same pool UUID, same lower tiers,
// same fsid). Client reconnects transparently — either the cached
// handle resolves (if placement_replay pre-instantiated the inode)
// or nfsd returns STALE_FH, the client re-resolves via LOOKUP, and
// the retry succeeds.
func TestE2ENFSReconnectAcrossServerUmount(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{plannerIntervalSec: 3600})
	_ = env.waitPool(10 * time.Second)

	const relPath = "reconnect-probe.txt"
	const payload = "phase4.3-reconnect-bytes\n"
	probePath := filepath.Join(env.mountpoint, relPath)
	if err := os.WriteFile(probePath, []byte(payload), 0o644); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	unix.Sync()

	clientMnt := nfsExportSetup(t, env, "22222222-3333-4444-5555-666666666666")
	nfsPath := filepath.Join(clientMnt, relPath)

	// Prime the client-side handle cache with a read.
	if got, err := os.ReadFile(nfsPath); err != nil {
		t.Fatalf("pre-remount NFS read: %v", err)
	} else if string(got) != payload {
		t.Fatalf("pre-remount NFS read = %q, want %q", got, payload)
	}

	// Server-side churn: unexport → stop tierd → umount smoothfs →
	// start tierd → mount smoothfs → re-export. Lower tiers stay
	// mounted; the pool UUID, tier paths, and /etc/exports fsid= are
	// all identical so the kernel recomputes the same sbi->fsid and
	// nfsd's export entry is byte-identical to what the client cached
	// a handle against.
	if out, err := exec.Command("exportfs", "-u", "127.0.0.1:"+env.mountpoint).CombinedOutput(); err != nil {
		t.Fatalf("exportfs -u: %s: %v", out, err)
	}
	env.stopService()
	// env.unmount() is non-lazy and swallows EBUSY; nfsd's filecache
	// takes a beat to release after exportfs -u, so env.mount()
	// would stack a second mount on top of the still-held old sb.
	// umount -l here forces detach so the remount is clean.
	if out, err := exec.Command("umount", "-l", env.mountpoint).CombinedOutput(); err != nil {
		t.Fatalf("umount -l mountpoint: %s: %v", out, err)
	}
	env.startService()
	env.mount()
	_ = env.waitPool(10 * time.Second)
	if out, err := exec.Command("exportfs", "-ra").CombinedOutput(); err != nil {
		t.Fatalf("exportfs -ra: %s: %v", out, err)
	}

	// The file still resolves by name. This may internally take one
	// STALE_FH bounce (client uses cached handle → ESTALE → re-LOOKUP)
	// but nfs-mount is hard by default so the retry is transparent.
	// NFS timeout is 60s/major, so cap our wait at 20s.
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(nfsPath)
		if err == nil && string(got) == payload {
			return
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("post-remount NFS read never succeeded (last err: %v)", last)
}

// TestE2ENFSReconnectAcrossModuleReload — Phase 4.3 gate #2. Stronger
// than the umount test: server does rmmod + modprobe between. The
// smoothfs sb is a completely fresh kmem_cache allocation, all in-
// kernel oid_map state is gone, the placement log replay path is the
// ONLY thing that could pre-populate. Ensures module-reload-driven
// kernel updates don't break open NFS sessions.
func TestE2ENFSReconnectAcrossModuleReload(t *testing.T) {
	// Module reload requires the env to cleanly unload between stop
	// and start. The existing env.stopService+unmount sequence leaves
	// the module loaded; we do the reload between those and the
	// corresponding start+mount.
	env := newE2EEnv(t, e2eConfig{plannerIntervalSec: 3600})
	_ = env.waitPool(10 * time.Second)

	const relPath = "reload-probe.txt"
	const payload = "phase4.3-module-reload\n"
	probePath := filepath.Join(env.mountpoint, relPath)
	if err := os.WriteFile(probePath, []byte(payload), 0o644); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	unix.Sync()

	clientMnt := nfsExportSetup(t, env, "33333333-4444-5555-6666-777777777777")
	nfsPath := filepath.Join(clientMnt, relPath)

	if got, err := os.ReadFile(nfsPath); err != nil {
		t.Fatalf("pre-reload NFS read: %v", err)
	} else if string(got) != payload {
		t.Fatalf("pre-reload NFS read = %q, want %q", got, payload)
	}

	// Full cycle: unexport, umount smoothfs (lazy — nfsd's filecache
	// hold is released async after exportfs -u, so non-lazy would
	// EBUSY), rmmod, modprobe, mount smoothfs, re-export.
	if out, err := exec.Command("exportfs", "-u", "127.0.0.1:"+env.mountpoint).CombinedOutput(); err != nil {
		t.Fatalf("exportfs -u: %s: %v", out, err)
	}
	env.stopService()
	if out, err := exec.Command("umount", "-l", env.mountpoint).CombinedOutput(); err != nil {
		t.Fatalf("umount -l mountpoint: %s: %v", out, err)
	}
	// rmmod still requires refcount==0. nfsd's filecache eviction
	// is async; give it a moment, then retry.
	deadlineRmmod := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadlineRmmod) {
		if out, err := exec.Command("rmmod", "smoothfs").CombinedOutput(); err == nil {
			break
		} else if !strings.Contains(string(out), "in use") {
			t.Fatalf("rmmod smoothfs: %s: %v", out, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	loadSmoothfsModule(t)
	env.startService()
	env.mount()
	_ = env.waitPool(10 * time.Second)
	if out, err := exec.Command("exportfs", "-ra").CombinedOutput(); err != nil {
		t.Fatalf("exportfs -ra: %s: %v", out, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(nfsPath)
		if err == nil && string(got) == payload {
			return
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("post-reload NFS read never succeeded (last err: %v)", last)
}

// smbShareSetup stands up an isolated smbd (loopback-only, port 8445,
// all tdb / lock / log state under env.root) serving env.mountpoint
// as the [smoothfs] share, then mounts it back via CIFS so tests can
// exercise the path without racing any system-installed Samba.
//
// Returns the CIFS client mount path. Cleanup is registered with t.
func smbShareSetup(t *testing.T, env *e2eEnv) (clientMnt string) {
	t.Helper()
	for _, tool := range []string{"smbd", "smbpasswd", "smbclient", "mount.cifs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing %s — install samba / smbclient / cifs-utils", tool)
		}
	}
	// Free port 445 and the tdb state dirs from any packaged Samba.
	for _, svc := range []string{"smbd", "nmbd", "samba", "samba-ad-dc"} {
		_ = exec.Command("systemctl", "stop", svc).Run()
	}

	sambaDir := filepath.Join(env.root, "samba")
	privateDir := filepath.Join(sambaDir, "private")
	if err := os.MkdirAll(privateDir, 0o755); err != nil {
		t.Fatalf("mkdir samba state: %v", err)
	}

	const (
		port    = 8445
		share   = "smoothfs"
		smbUser = "smbtest"
		smbPass = "smbtest-e2e"
	)
	smbConf := filepath.Join(sambaDir, "smb.conf")
	conf := fmt.Sprintf(`[global]
    workgroup = WORKGROUP
    server string = smoothfs e2e
    server role = standalone server
    log level = 1
    log file = %s/log.%%m
    pid directory = %s
    lock directory = %s
    state directory = %s
    cache directory = %s
    private dir = %s
    passdb backend = tdbsam:%s/passdb.tdb
    smb ports = %d
    bind interfaces only = yes
    interfaces = lo
    map to guest = never
    disable spoolss = yes
    load printers = no
    printing = bsd
    printcap name = /dev/null
    ea support = yes

[%s]
    path = %s
    read only = no
    guest ok = no
    valid users = %s
    force user = root
    ea support = yes
    create mask = 0644
    directory mask = 0755
`, sambaDir, sambaDir, sambaDir, sambaDir, sambaDir,
		privateDir, sambaDir, port,
		share, env.mountpoint, smbUser)
	if err := os.WriteFile(smbConf, []byte(conf), 0o644); err != nil {
		t.Fatalf("write smb.conf: %v", err)
	}

	// Ensure a unix user exists so Samba's CHECK_UID gate is happy.
	_ = exec.Command("useradd", "--no-create-home",
		"--shell", "/usr/sbin/nologin", smbUser).Run()

	pwCmd := exec.Command("smbpasswd", "-c", smbConf, "-a", "-s", smbUser)
	pwCmd.Stdin = strings.NewReader(smbPass + "\n" + smbPass + "\n")
	if out, err := pwCmd.CombinedOutput(); err != nil {
		t.Fatalf("smbpasswd: %s: %v", strings.TrimSpace(string(out)), err)
	}

	smbdLog, err := os.Create(filepath.Join(sambaDir, "smbd.stdout"))
	if err != nil {
		t.Fatalf("create smbd.stdout: %v", err)
	}
	smbdCmd := exec.Command("smbd", "--foreground", "--no-process-group",
		"--configfile="+smbConf, "--debug-stdout")
	smbdCmd.Stdout = smbdLog
	smbdCmd.Stderr = smbdLog
	if err := smbdCmd.Start(); err != nil {
		smbdLog.Close()
		t.Fatalf("start smbd: %v", err)
	}
	// smbd forks per-connection children that inherit stdout/stderr
	// from the master. cmd.Wait would then block on the Cmd.copyWait
	// goroutines draining those inherited pipes until every child
	// exits. pkill'ing the whole family (master + children) first
	// makes Wait return promptly; the 3s ceiling is a last-resort
	// in case a child is stuck in D-state on a kernel bug.
	t.Cleanup(func() {
		_ = exec.Command("pkill", "-9", "-f", "smbd.*"+smbConf).Run()
		if smbdCmd.Process != nil {
			_ = smbdCmd.Process.Kill()
			done := make(chan struct{})
			go func() { _ = smbdCmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
		}
		smbdLog.Close()
	})

	// Wait for smbd to listen (up to 10s).
	listening := false
	for i := 0; i < 50; i++ {
		out, _ := exec.Command("ss", "-lnt").Output()
		if strings.Contains(string(out), fmt.Sprintf(":%d ", port)) {
			listening = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !listening {
		tail, _ := os.ReadFile(filepath.Join(sambaDir, "smbd.stdout"))
		t.Fatalf("smbd never bound port %d; stdout tail:\n%s", port, tail)
	}

	clientMnt = filepath.Join(env.root, "cifs-client")
	if err := os.MkdirAll(clientMnt, 0o755); err != nil {
		t.Fatalf("mkdir cifs client: %v", err)
	}
	mountOpts := fmt.Sprintf("port=%d,username=%s,password=%s,vers=3.0,cache=none,noperm",
		port, smbUser, smbPass)
	src := fmt.Sprintf("//127.0.0.1/%s", share)
	if out, err := exec.Command("mount", "-t", "cifs", src, clientMnt,
		"-o", mountOpts).CombinedOutput(); err != nil {
		t.Fatalf("mount.cifs: %s: %v", strings.TrimSpace(string(out)), err)
	}
	t.Cleanup(func() {
		_ = exec.Command("umount", "-l", clientMnt).Run()
	})
	return clientMnt
}

// TestE2ESMBMovementAcrossOpenFD — Phase 5.2 gate. SMB analog of
// TestE2ENFSMovementAcrossOpenFD: a long-lived fd opened via a CIFS
// mount must survive a MOVE_PLAN + MOVE_CUTOVER underneath, with
// reads continuing to return the right bytes and writes landing on
// the new lower tier. The Phase 4 per-fd reissue protocol was
// protocol-agnostic (it runs inside smoothfs_lower_file), but this
// test pins that invariant against the SMB path so a future lower-
// level regression that slipped past the NFS test would still trip.
//
// No Samba VFS module yet (Phase 5.3); Phase 5.2 proves the surface
// is already good with stock Samba.
func TestE2ESMBMovementAcrossOpenFD(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{
		plannerIntervalSec: 3600, // planner effectively off
		minResidencySec:    0,
		cooldownSec:        0,
	})
	ctl := openE2EClient(t)
	_ = env.waitPool(10 * time.Second)

	// Seed the file via the smoothfs mount, register it in SQLite.
	const relPath = "smb-movement-probe.txt"
	initial := []byte("phase5.2-initial-content\n")
	probePath := filepath.Join(env.mountpoint, relPath)
	if err := os.WriteFile(probePath, initial, 0o644); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	unix.Sync()
	oid := env.seedMountedObject(relPath, "tier-fast")

	// Stand up Samba + CIFS client mount over the smoothfs mount.
	clientMnt := smbShareSetup(t, env)
	smbPath := filepath.Join(clientMnt, relPath)

	// Open fd via CIFS for read+write. Hold it across the cutover.
	smbFile, err := os.OpenFile(smbPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("CIFS open: %v", err)
	}
	t.Cleanup(func() { smbFile.Close() })

	// Pre-cutover read sanity.
	got := make([]byte, len(initial))
	if _, err := io.ReadFull(smbFile, got); err != nil {
		t.Fatalf("pre-cutover CIFS read: %v", err)
	}
	if !bytes.Equal(got, initial) {
		t.Fatalf("pre-cutover read = %q, want %q", got, initial)
	}

	// Drive movement directly: MOVE_PLAN to slow, stage destination
	// (tierd worker would do this), MOVE_CUTOVER.
	if err := ctl.MovePlan(env.poolUUID, oid, 1, 100); err != nil {
		t.Fatalf("MOVE_PLAN: %v", err)
	}
	dstPath := filepath.Join(env.slowMount, relPath)
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		t.Fatalf("mkdir slow parent: %v", err)
	}
	if err := os.WriteFile(dstPath, initial, 0o644); err != nil {
		t.Fatalf("stage slow copy: %v", err)
	}
	if err := unix.Setxattr(dstPath, "trusted.smoothfs.oid", oid[:], 0); err != nil {
		t.Fatalf("set slow oid xattr: %v", err)
	}
	if err := ctl.MoveCutover(env.poolUUID, oid, 100); err != nil {
		t.Fatalf("MOVE_CUTOVER: %v", err)
	}

	if _, err := smbFile.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("post-cutover seek: %v", err)
	}
	if _, err := io.ReadFull(smbFile, got); err != nil {
		t.Fatalf("post-cutover CIFS read: %v", err)
	}
	if !bytes.Equal(got, initial) {
		t.Fatalf("post-cutover read = %q, want %q", got, initial)
	}

	// Post-cutover write through the SAME fd. Should land on the
	// slow tier via the reissued lower_file.
	appended := []byte("APPEND-AFTER-SMB-CUTOVER\n")
	if _, err := smbFile.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("seek end: %v", err)
	}
	if _, err := smbFile.Write(appended); err != nil {
		t.Fatalf("post-cutover CIFS write: %v", err)
	}
	if err := smbFile.Sync(); err != nil {
		t.Fatalf("cifs fsync: %v", err)
	}
	unix.Sync()

	want := append(append([]byte{}, initial...), appended...)
	if slowGot, err := os.ReadFile(dstPath); err != nil {
		t.Fatalf("read final from slow tier: %v", err)
	} else if !bytes.Equal(slowGot, want) {
		t.Fatalf("slow tier final = %q, want %q", slowGot, want)
	}
	if smoothGot, err := os.ReadFile(probePath); err != nil {
		t.Fatalf("read final via smoothfs mount: %v", err)
	} else if !bytes.Equal(smoothGot, want) {
		t.Fatalf("smoothfs mount final = %q, want %q", smoothGot, want)
	}
	// CIFS stat goes through the Samba lookup/getattr path,
	// exercising attribute propagation post-cutover. If the upper
	// inode held stale source-tier attrs we'd see the pre-cutover
	// size here.
	if st, err := os.Stat(smbPath); err != nil {
		t.Fatalf("post-cutover CIFS stat: %v", err)
	} else if st.Size() != int64(len(want)) {
		t.Fatalf("post-cutover CIFS stat size = %d, want %d", st.Size(), len(want))
	}
}

// TestE2ESMBLeasePinSkipsMovement — Phase 5.2 gate #2. With
// trusted.smoothfs.lease set on a file via a Samba-mediated path,
// MOVE_PLAN to a different tier must be refused (EBUSY). Clearing
// the lease must un-pin the file so the next MOVE_PLAN succeeds.
// This is the runtime half of the contract Phase 5.0 wired; the
// Samba VFS module in 5.3 will toggle the xattr on lease grant /
// break, and this test locks in that the kernel does what the
// module expects.
func TestE2ESMBLeasePinSkipsMovement(t *testing.T) {
	env := newE2EEnv(t, e2eConfig{
		plannerIntervalSec: 3600,
		minResidencySec:    0,
		cooldownSec:        0,
	})
	ctl := openE2EClient(t)
	_ = env.waitPool(10 * time.Second)

	const relPath = "smb-lease-probe.txt"
	probePath := filepath.Join(env.mountpoint, relPath)
	if err := os.WriteFile(probePath, []byte("phase5.2-lease\n"), 0o644); err != nil {
		t.Fatalf("seed probe: %v", err)
	}
	unix.Sync()
	oid := env.seedMountedObject(relPath, "tier-fast")

	// Set the lease pin the same way a Samba VFS module will.
	if err := unix.Setxattr(probePath, "trusted.smoothfs.lease",
		[]byte{1}, 0); err != nil {
		t.Fatalf("set lease xattr: %v", err)
	}

	// Confirm inspect reports PIN_LEASE.
	res, err := ctl.Inspect(env.poolUUID, oid)
	if err != nil {
		t.Fatalf("inspect (leased): %v", err)
	}
	if res.PinState != PinLease {
		t.Fatalf("pin_state = %q, want %q", res.PinState, PinLease)
	}

	// MOVE_PLAN must be refused while leased. The kernel returns
	// -EBUSY per the §0.7 scheduler skip contract; the exact error
	// surface that reaches the Go client is netlink-formatted and
	// varies with kernel ACK shape, so we only care that the call
	// returns a non-nil error and that no placement state changed
	// as a side effect.
	err = ctl.MovePlan(env.poolUUID, oid, 1, 200)
	if err == nil {
		t.Fatal("MOVE_PLAN succeeded while lease-pinned — pin_state skip is not gating movement")
	}
	t.Logf("MOVE_PLAN correctly refused while leased: %v", err)

	// The refused MOVE_PLAN must not have advanced state. Use a
	// fresh client because a refused netlink transaction can wedge
	// the first client's seq counter.
	ctl2 := openE2EClient(t)
	res, err = ctl2.Inspect(env.poolUUID, oid)
	if err != nil {
		t.Fatalf("inspect (post-refused-plan): %v", err)
	}
	if res.PinState != PinLease {
		t.Fatalf("pin_state after refused plan = %q, want %q", res.PinState, PinLease)
	}
	if res.MovementState != StatePlaced {
		t.Fatalf("movement_state after refused plan = %q, want %q",
			res.MovementState, StatePlaced)
	}
	if res.IntendedTier != 0 {
		t.Fatalf("intended_tier after refused plan = %d, want 0",
			res.IntendedTier)
	}

	// Drop the lease. Xattr-level clear must take pin_state back to
	// PIN_NONE so the scheduler is free to plan a move again. The
	// actual move-now-succeeds path is covered by the phase 4.2
	// NFS-side test; we only re-verify the pin transition here.
	if err := unix.Removexattr(probePath, "trusted.smoothfs.lease"); err != nil {
		t.Fatalf("remove lease xattr: %v", err)
	}
	ctl3 := openE2EClient(t)
	res, err = ctl3.Inspect(env.poolUUID, oid)
	if err != nil {
		t.Fatalf("inspect (unleased): %v", err)
	}
	if res.PinState != PinNone {
		t.Fatalf("post-clear pin_state = %q, want %q", res.PinState, PinNone)
	}
}

// TestE2ESMBForcedMoveBreaksLease — Phase 5.3 gate. Exercises the
// kernel side of the Samba lease-break story end-to-end:
//
//  1. Set trusted.smoothfs.lease on a file (what the Samba VFS module
//     will do when SMB grants a lease).
//  2. Spawn the reference lease_break_agent — a fanotify listener
//     standing in for what the Samba VFS module will do in 5.4+.
//  3. Normal MOVE_PLAN must still refuse while the pin is held.
//  4. Forced MOVE_PLAN (force=true) must succeed, the cutover must
//     fire fsnotify(FS_MODIFY), the agent must see the event and
//     removexattr the lease, and pin_state must end at PIN_NONE.
//
// This is the smoothfs half of the contract; the Samba VFS module
// in the smoothfs-samba-vfs-module proposal plugs the same xattr
// toggle + event handler into SMB_VFS_SET_LEASE.
func TestE2ESMBForcedMoveBreaksLease(t *testing.T) {
	agentBin := os.Getenv("SMOOTHFS_LEASE_BREAK_AGENT")
	if agentBin == "" {
		t.Skip("set SMOOTHFS_LEASE_BREAK_AGENT to the built lease_break_agent path")
	}
	if _, err := os.Stat(agentBin); err != nil {
		t.Skipf("lease_break_agent at %s: %v", agentBin, err)
	}

	env := newE2EEnv(t, e2eConfig{
		plannerIntervalSec: 3600,
		minResidencySec:    0,
		cooldownSec:        0,
	})
	ctl := openE2EClient(t)
	_ = env.waitPool(10 * time.Second)

	const relPath = "forced-move-probe.txt"
	probePath := filepath.Join(env.mountpoint, relPath)
	if err := os.WriteFile(probePath, []byte("phase5.3-forced\n"), 0o644); err != nil {
		t.Fatalf("seed probe: %v", err)
	}
	unix.Sync()
	oid := env.seedMountedObject(relPath, "tier-fast")

	// Install the lease before starting the agent so we don't race
	// the agent catching its own setxattr as an event.
	if err := unix.Setxattr(probePath, "trusted.smoothfs.lease",
		[]byte{1}, 0); err != nil {
		t.Fatalf("set lease xattr: %v", err)
	}

	// Start the reference agent. It watches the smoothfs mount for
	// FAN_MODIFY events and clears trusted.smoothfs.lease on the
	// affected file.
	agentLog, err := os.Create(filepath.Join(env.root, "agent.log"))
	if err != nil {
		t.Fatalf("create agent log: %v", err)
	}
	agentCmd := exec.Command(agentBin, env.mountpoint, "-v")
	agentCmd.Stdout = agentLog
	agentCmd.Stderr = agentLog
	if err := agentCmd.Start(); err != nil {
		agentLog.Close()
		t.Fatalf("start lease_break_agent: %v", err)
	}
	t.Cleanup(func() {
		if agentCmd.Process != nil {
			_ = agentCmd.Process.Signal(syscall.SIGTERM)
			_ = agentCmd.Wait()
		}
		agentLog.Close()
	})
	// Give the agent a beat to finish fanotify_init+mark.
	time.Sleep(200 * time.Millisecond)

	// Un-forced MOVE_PLAN must still refuse (PIN_LEASE in place).
	if err := ctl.MovePlan(env.poolUUID, oid, 1, 300); err == nil {
		t.Fatal("unforced MOVE_PLAN succeeded while lease-pinned — force bypass is leaking to the non-force path")
	}

	// Forced plan. Kernel bypasses the PIN_LEASE pin only.
	ctl2 := openE2EClient(t)
	if err := ctl2.MovePlanForce(env.poolUUID, oid, 1, 301, true); err != nil {
		t.Fatalf("forced MOVE_PLAN: %v", err)
	}

	// Stage slow copy the way tierd's worker would.
	dstPath := filepath.Join(env.slowMount, relPath)
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		t.Fatalf("mkdir slow parent: %v", err)
	}
	if err := os.WriteFile(dstPath, []byte("phase5.3-forced\n"), 0o644); err != nil {
		t.Fatalf("stage slow copy: %v", err)
	}
	if err := unix.Setxattr(dstPath, "trusted.smoothfs.oid", oid[:], 0); err != nil {
		t.Fatalf("set slow oid xattr: %v", err)
	}

	// Cutover fires FS_MODIFY on the smoothfs inode (movement.c), the
	// agent picks it up and removexattr's the lease.
	if err := ctl2.MoveCutover(env.poolUUID, oid, 301); err != nil {
		t.Fatalf("MOVE_CUTOVER: %v", err)
	}

	// Poll for the agent's side effect. The xattr is always readable
	// (handler computes it from pin_state), so we poll on the byte
	// value — 1 before the agent reacts, 0 after its removexattr
	// drops pin_state back to PIN_NONE. Bounded at 5s so a stuck
	// agent fails the test loudly instead of hanging.
	deadline := time.Now().Add(5 * time.Second)
	cleared := false
	got := make([]byte, 1)
	for time.Now().Before(deadline) {
		sz, err := unix.Getxattr(probePath, "trusted.smoothfs.lease", got)
		if err == nil && sz == 1 && got[0] == 0 {
			cleared = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !cleared {
		t.Fatalf("lease xattr never cleared within deadline (last got=%v)", got)
	}

	// And the kernel's own pin_state should already be PIN_NONE,
	// cleared by smoothfs_movement_cutover when it fired fsnotify.
	ctl3 := openE2EClient(t)
	res, err := ctl3.Inspect(env.poolUUID, oid)
	if err != nil {
		t.Fatalf("inspect after forced cutover: %v", err)
	}
	if res.PinState != PinNone {
		t.Fatalf("pin_state after forced cutover = %q, want %q",
			res.PinState, PinNone)
	}
	if res.MovementState != StateSwitched {
		t.Fatalf("movement_state after forced cutover = %q, want %q",
			res.MovementState, StateSwitched)
	}
}

type e2eConfig struct {
	plannerIntervalSec int
	minResidencySec    int
	cooldownSec        int
}

type latencyStats struct {
	P50 time.Duration
	P95 time.Duration
	P99 time.Duration
}

type ioPerfResult struct {
	Path           string
	ThroughputMBps float64
	CPUSeconds     float64
}

func newE2EEnv(t *testing.T, cfg e2eConfig) *e2eEnv {
	return newE2EEnvWithFS(t, cfg, lowerFSXFS, nil)
}

func newE2EEnvWithFS(t *testing.T, cfg e2eConfig, fs lowerFSSpec, setup func(t *testing.T, env *e2eEnv)) *e2eEnv {
	t.Helper()
	ensureE2EPrereqs(t, fs)

	root := t.TempDir()
	sqlDB := newE2EDB(t, filepath.Join(root, "tierd.db"))
	applyPlannerOverrides(t, sqlDB, cfg)

	fastLoop, fastMount := makeLoopbackFS(t, root, "fast.img", "fast", fs)
	slowLoop, slowMount := makeLoopbackFS(t, root, "slow.img", "slow", fs)

	runBestEffort("", "rmmod", "smoothfs")
	loadSmoothfsModule(t)

	poolUUID := uuid.New()
	execSQL(t, sqlDB, `INSERT INTO placement_domains (id, backend_kind) VALUES (?, ?)`, "domain-e2e", "smoothfs")
	execSQL(t, sqlDB, `INSERT INTO tier_targets
	        (id, name, placement_domain, backend_kind, rank, backing_ref)
	        VALUES (?, ?, ?, ?, ?, ?)`, "tier-fast", "fast", "domain-e2e", "smoothfs", 0, fastMount)
	execSQL(t, sqlDB, `INSERT INTO tier_targets
	        (id, name, placement_domain, backend_kind, rank, backing_ref)
	        VALUES (?, ?, ?, ?, ?, ?)`, "tier-slow", "slow", "domain-e2e", "smoothfs", 1, slowMount)
	execSQL(t, sqlDB, `INSERT INTO managed_namespaces
	        (id, name, placement_domain, backend_kind, backend_ref)
	        VALUES (?, ?, ?, ?, ?)`, "ns-e2e", "e2e", "domain-e2e", "smoothfs", poolUUID.String())

	env := &e2eEnv{
		t:          t,
		root:       root,
		sqlDB:      sqlDB,
		poolUUID:   poolUUID,
		fastMount:  fastMount,
		slowMount:  slowMount,
		mountpoint: filepath.Join(root, "smoothfs"),
	}
	if err := os.MkdirAll(env.mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir mountpoint: %v", err)
	}
	if setup != nil {
		setup(t, env)
	}
	env.startService()
	env.mount()

	t.Cleanup(func() {
		env.stopService()
		env.unmount()
		runBestEffort("", "umount", "-l", env.fastMount)
		runBestEffort("", "umount", "-l", env.slowMount)
		detachLoop(t, fastLoop)
		detachLoop(t, slowLoop)
	})
	return env
}

func ensureE2EPrereqs(t *testing.T, fs lowerFSSpec) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	if os.Getenv("SMOOTHFS_KO") == "" {
		t.Skip("set SMOOTHFS_KO to the built smoothfs.ko path")
	}
	tools := []string{"losetup", fs.mkfsTool, "mount", "umount", "insmod", "rmmod", "truncate"}
	if fs.name == "btrfs" {
		tools = append(tools, "btrfs")
	}
	for _, tool := range tools {
		requireTool(t, tool)
	}
}

func applyPlannerOverrides(t *testing.T, sqlDB *sql.DB, cfg e2eConfig) {
	t.Helper()
	updates := map[string]int{
		"smoothfs_planner_interval_seconds":  cfg.plannerIntervalSec,
		"smoothfs_min_residency_seconds":     cfg.minResidencySec,
		"smoothfs_movement_cooldown_seconds": cfg.cooldownSec,
	}
	for key, value := range updates {
		execSQL(t, sqlDB, `UPDATE control_plane_config SET value = ? WHERE key = ?`, fmt.Sprintf("%d", value), key)
	}
}

func (e *e2eEnv) startService() {
	e.t.Helper()
	e.ctx, e.cancel = context.WithTimeout(context.Background(), 60*time.Second)
	e.done = make(chan struct{})
	svc, err := NewService(e.ctx, e.sqlDB, 1)
	if err != nil {
		e.t.Fatalf("NewService: %v", err)
	}
	e.svc = svc
	go func() {
		defer close(e.done)
		_ = svc.Run(e.ctx)
	}()
}

func (e *e2eEnv) stopService() {
	e.t.Helper()
	if e.cancel != nil {
		e.cancel()
	}
	if e.svc != nil {
		_ = e.svc.Close()
		e.svc = nil
	}
	if e.done != nil {
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			e.t.Fatalf("timed out waiting for service shutdown")
		}
	}
	e.cancel = nil
	e.done = nil
}

func (e *e2eEnv) mount() {
	e.t.Helper()
	run(e.t, "", "mount", "-t", "smoothfs",
		"-o", fmt.Sprintf("pool=e2e,uuid=%s,tiers=%s:%s", e.poolUUID, e.fastMount, e.slowMount),
		"none", e.mountpoint)
}

func (e *e2eEnv) unmount() {
	e.t.Helper()
	runBestEffort("", "umount", e.mountpoint)
}

func (e *e2eEnv) waitPool(timeout time.Duration) *Pool {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.svc != nil {
			pool := e.svc.PoolByUUID(e.poolUUID.String())
			if pool != nil {
				return pool
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("pool %s was not auto-registered within %v", e.poolUUID, timeout)
	return nil
}

func (e *e2eEnv) seedMountedObject(relPath, currentTierID string) [OIDLen]byte {
	e.t.Helper()
	oid, err := readOIDXattr(filepath.Join(e.fastMount, relPath))
	if err != nil {
		e.t.Fatalf("read object oid xattr: %v", err)
	}
	execSQL(e.t, e.sqlDB, `INSERT INTO smoothfs_objects
	        (object_id, namespace_id, current_tier_id, rel_path)
	        VALUES (?, ?, ?, ?)`,
		hex.EncodeToString(oid[:]), "ns-e2e", currentTierID, relPath)
	return oid
}

func readOIDXattr(path string) ([OIDLen]byte, error) {
	var oid [OIDLen]byte
	// smoothfs defers trusted.smoothfs.oid xattr writes from CREATE
	// to a per-sb workqueue for latency; a call through to the lower
	// here (the test harness's whole point is to verify the xattr
	// landed on the lower) needs to flush the WB queue first. sync(2)
	// fans out to sync_fs on every sb including smoothfs, which
	// drains the queue synchronously.
	unix.Sync()
	buf := make([]byte, 64)
	n, err := unix.Getxattr(path, "trusted.smoothfs.oid", buf)
	if err != nil {
		return oid, err
	}
	if n != OIDLen {
		return oid, fmt.Errorf("trusted.smoothfs.oid length = %d, want %d", n, OIDLen)
	}
	copy(oid[:], buf[:n])
	return oid, nil
}

func (e *e2eEnv) waitEWMA(oid [OIDLen]byte, timeout time.Duration) float64 {
	e.t.Helper()
	oidHex := hex.EncodeToString(oid[:])
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var ewma float64
		err := e.sqlDB.QueryRow(`SELECT ewma_value FROM smoothfs_objects WHERE object_id = ?`, oidHex).Scan(&ewma)
		if err == nil && ewma > 0 {
			return ewma
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("ewma for %s did not become positive within %v", oidHex, timeout)
	return 0
}

func (e *e2eEnv) waitCurrentTier(oid [OIDLen]byte, tierID string, timeout time.Duration) {
	e.t.Helper()
	oidHex := hex.EncodeToString(oid[:])
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var current string
		err := e.sqlDB.QueryRow(`SELECT current_tier_id FROM smoothfs_objects WHERE object_id = ?`, oidHex).Scan(&current)
		if err == nil && current == tierID {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("object %s did not reach tier %s within %v", oidHex, tierID, timeout)
}

func (e *e2eEnv) waitInspectState(oid [OIDLen]byte, state MovementState, currentTier uint8, timeout time.Duration) *InspectResult {
	return e.waitInspectStateWithClient(e.svc.ClientConn(), oid, state, currentTier, timeout)
}

func (e *e2eEnv) waitInspectStateWithClient(client *Client, oid [OIDLen]byte, state MovementState, currentTier uint8, timeout time.Duration) *InspectResult {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ins, err := client.Inspect(e.poolUUID, oid)
		if err == nil && ins.MovementState == state && ins.CurrentTier == currentTier {
			return ins
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("inspect for %s did not reach state=%s current_tier=%d within %v",
		hex.EncodeToString(oid[:]), state, currentTier, timeout)
	return nil
}

func openE2EClient(t *testing.T) *Client {
	t.Helper()
	client, err := Open()
	if err != nil {
		t.Fatalf("open smoothfs client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func (e *e2eEnv) waitDBPlacedState(oid [OIDLen]byte, tierID string, timeout time.Duration) {
	e.t.Helper()
	oidHex := hex.EncodeToString(oid[:])
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var current string
		var intended sql.NullString
		var state string
		err := e.sqlDB.QueryRow(`
			SELECT current_tier_id, intended_tier_id, movement_state
			  FROM smoothfs_objects
			 WHERE object_id = ?`, oidHex).Scan(&current, &intended, &state)
		if err == nil && current == tierID && state == "placed" && !intended.Valid {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("db row for %s did not reach placed@%s within %v", oidHex, tierID, timeout)
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("missing required tool %s", name)
	}
}

func newE2EDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	store, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store.DB()
}

func makeLoopbackFS(t *testing.T, root, imageName, mountName string, fs lowerFSSpec) (loopdev, mountpoint string) {
	t.Helper()
	img := filepath.Join(root, imageName)
	run(t, "", "truncate", "-s", fs.imageSize, img)

	loopdev = runOutput(t, "", "losetup", "--find", "--show", img)
	runArgs := append([]string{}, fs.mkfsArgs...)
	runArgs = append(runArgs, loopdev)
	run(t, "", fs.mkfsTool, runArgs...)

	mountpoint = filepath.Join(root, mountName)
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir mountpoint %s: %v", mountpoint, err)
	}
	if fs.mountType != "" {
		run(t, "", "mount", "-t", fs.mountType, loopdev, mountpoint)
	} else {
		run(t, "", "mount", loopdev, mountpoint)
	}
	t.Cleanup(func() { runBestEffort("", "umount", "-l", mountpoint) })
	return loopdev, mountpoint
}

func detachLoop(t *testing.T, loopdev string) {
	t.Helper()
	if loopdev == "" {
		return
	}
	runBestEffort("", "losetup", "-d", loopdev)
}

func execSQL(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", oneLine(q), err)
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

func runOutput(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
	return string(bytesTrimSpace(out))
}

func runBestEffort(dir string, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	_, _ = cmd.CombinedOutput()
}

func loadSmoothfsModule(t *testing.T) {
	t.Helper()
	cmd := exec.Command("insmod", os.Getenv("SMOOTHFS_KO"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	if strings.Contains(string(out), "File exists") {
		return
	}
	t.Fatalf("insmod [%s] failed: %v\n%s", os.Getenv("SMOOTHFS_KO"), err, out)
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == '\n' || b[0] == '\r' || b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

func benchmarkCreateLatency(t *testing.T, root string, payload []byte, rounds, filesPerRound int) latencyStats {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	latencies := make([]time.Duration, 0, rounds*filesPerRound)
	for round := 0; round < rounds; round++ {
		roundDir := filepath.Join(root, fmt.Sprintf("round-%02d", round))
		if err := os.MkdirAll(roundDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", roundDir, err)
		}
		for fileIdx := 0; fileIdx < filesPerRound; fileIdx++ {
			path := filepath.Join(roundDir, fmt.Sprintf("file-%04d.bin", fileIdx))
			start := time.Now()
			if err := os.WriteFile(path, payload, 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			latencies = append(latencies, time.Since(start))
		}
	}
	return summarizeLatencies(t, latencies)
}

func benchmarkSequentialWrite(t *testing.T, path string, totalBytes int64) ioPerfResult {
	t.Helper()
	buf := bytes.Repeat([]byte("w"), 1<<20)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	cpuStart := currentCPUSeconds(t)
	start := time.Now()
	var written int64
	for written < totalBytes {
		chunk := int64(len(buf))
		if remaining := totalBytes - written; remaining < chunk {
			chunk = remaining
		}
		n, err := f.Write(buf[:chunk])
		if err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		written += int64(n)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("fsync %s: %v", path, err)
	}
	elapsed := time.Since(start)
	cpuElapsed := currentCPUSeconds(t) - cpuStart
	return ioPerfResult{
		Path:           path,
		ThroughputMBps: float64(totalBytes) / (1024 * 1024) / elapsed.Seconds(),
		CPUSeconds:     cpuElapsed,
	}
}

func benchmarkSequentialRead(t *testing.T, path string) ioPerfResult {
	t.Helper()
	readAll := func() {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		defer f.Close()
		buf := make([]byte, 1<<20)
		for {
			_, err := f.Read(buf)
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
		}
	}

	readAll() // warm-cache pass; the target is steady-state throughput.

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	cpuStart := currentCPUSeconds(t)
	start := time.Now()
	readAll()
	elapsed := time.Since(start)
	cpuElapsed := currentCPUSeconds(t) - cpuStart
	return ioPerfResult{
		Path:           path,
		ThroughputMBps: float64(info.Size()) / (1024 * 1024) / elapsed.Seconds(),
		CPUSeconds:     cpuElapsed,
	}
}

func benchmarkStatLatency(t *testing.T, path string, iterations int) latencyStats {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("meta"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	latencies := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		start := time.Now()
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		latencies = append(latencies, time.Since(start))
	}
	return summarizeLatencies(t, latencies)
}

func summarizeLatencies(t *testing.T, latencies []time.Duration) latencyStats {
	t.Helper()
	if len(latencies) == 0 {
		t.Fatal("no latency samples collected")
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return latencyStats{
		P50: percentileDuration(latencies, 0.50),
		P95: percentileDuration(latencies, 0.95),
		P99: percentileDuration(latencies, 0.99),
	}
}

func percentileDuration(latencies []time.Duration, pct float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	idx := int(float64(len(latencies)-1) * pct)
	return latencies[idx]
}

func currentCPUSeconds(t *testing.T) float64 {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return timevalSeconds(usage.Utime) + timevalSeconds(usage.Stime)
}

func timevalSeconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1_000_000
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
