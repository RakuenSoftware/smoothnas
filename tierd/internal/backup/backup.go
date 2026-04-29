// Package backup implements NFS/SMB backup operations using either cp+sha256
// hash verification or rsync.
package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/nfs"
	"golang.org/x/sys/unix"
)

var statPath = os.Stat
var statfsBackupPath = unix.Statfs

const (
	// copyBufSize is the per-goroutine buffer for io.CopyBuffer. 4 MB amortises
	// NFS RPC and smoothfs write overhead over large chunks, giving close to wire speed
	// on fast links without meaningful extra memory cost.
	copyBufSize = 4 * 1024 * 1024

	// copyWorkers is the number of files copied in parallel. Parallel copies hide
	// per-file RPC round-trip latency when the directory tree has many small files,
	// and keep the NFS link and local disk both busy simultaneously.
	copyWorkers = 4

	// dirWorkers is the number of directories created in parallel. os.MkdirAll
	// is safe to call concurrently on overlapping paths: racing goroutines that
	// need the same parent will create it themselves and the EEXIST from the
	// winner is silently absorbed. More workers hide the smoothfs round-trip
	// latency that makes sequential mkdir the dominant bottleneck on large trees.
	dirWorkers = 16

	// nfsMountOpts are the NFS mount options used for every backup source mount.
	//
	// nconnect is deliberately omitted. Operator history on this deployment
	// shows nconnect>1 reduced overall throughput on the real source NAS —
	// sticking to a single TCP connection per mount.
	//
	// rsize/wsize=2M: large RPC payloads amortise per-RPC overhead over fewer
	// round trips. 4M regressed on one real server; 2M is the safe upper bound.
	// Note: Linux NFS caps rsize/wsize at 1M by default (max_rsize); requesting
	// 2M is harmless and future-compatible when the server advertises more.
	//
	// vers=4.2: avoids the rpc.statd race that can produce "access denied" on
	// first mount when NFSv3 locking has not yet registered with statd.
	//
	// lookupcache=all + actimeo=60: cache directory entries and file attrs for
	// up to a minute, turning repeated stat() calls during rsync's tree walk
	// into local lookups instead of NFS RPCs.
	//
	// timeo=50,retrans=3: base RPC timeout of 5 s with three retries before TCP
	// reconnect (~35 s worst case). The kernel default of 60 s per RPC meant a
	// single unresponsive server stalled rsync for up to 3 minutes.
	nfsMountOpts = nfs.DefaultClientMountOptions

	// nfsReadAheadKB raises the per-mount BDI readahead so the kernel
	// pre-fetches further into a sequential NFS read stream. 4 MB is a
	// modest bump from the kernel default (~128 KB) — enough to keep the
	// server's prefetcher warm without risking memory pressure under load.
	nfsReadAheadKB = 4096

	// minDestFreeBytes is the lower bound on destination free space we
	// require before starting a backup. Below this we refuse to start —
	// the run would fail within seconds anyway and leave a confusing
	// partial-tree on the target.
	minDestFreeBytes = 1 << 30 // 1 GB

	// criticalDestFreeBytes is the in-flight floor. If a running backup's
	// destination drops below this, the watchdog cancels rsync so it exits
	// cleanly with our own error string instead of the kernel returning
	// ENOSPC mid-file. Enough headroom for rsync to wind down, journald to
	// keep logging, and the operator to diagnose.
	criticalDestFreeBytes = 200 << 20 // 200 MB
)

// destFreeBytes returns the number of bytes available to non-root on the
// filesystem containing path.
func destFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := statfsBackupPath(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func rsyncArchiveArgs(_ string) []string {
	// Keep the parts of archive mode that matter for NAS backups without
	// paying the NFS/ZFS cost of replaying source uid/gid/mode metadata.
	return []string{"-rltW", "--links", "--inplace",
		"--omit-dir-times", "--no-perms", "--no-owner", "--no-group",
		"--timeout=60",
		"--stats", "--no-human-readable", "--info=progress2",
	}
}

var rsyncSupportsOpenNoAtime = sync.OnceValue(func() bool {
	out, err := exec.Command("rsync", "--help").Output()
	return err == nil && bytes.Contains(out, []byte("--open-noatime"))
})

func rsyncMountArgs(dst string) []string {
	args := rsyncArchiveArgs(dst)
	// Mounted-source backups run rsync locally as root. When supported, avoid
	// updating source atime on every file open; on NFS that prevents a read-only
	// backup from producing avoidable metadata RPC/writeback work on the source.
	if rsyncSupportsOpenNoAtime() {
		args = append(args, "--open-noatime")
	}
	return args
}

// watchDestFree polls path's free space every 3s. If it drops below
// criticalDestFreeBytes, cancel is invoked (which cancels the rsync context)
// and the reason is delivered via reasonOut. Returns when ctx is done.
func watchDestFree(ctx context.Context, path string, cancel context.CancelFunc, reasonOut chan<- string) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			free, err := destFreeBytes(path)
			if err != nil {
				continue
			}
			if free < criticalDestFreeBytes {
				select {
				case reasonOut <- fmt.Sprintf("destination %s has only %d MB free (< %d MB floor); aborting to avoid a silent mid-file ENOSPC", path, free>>20, criticalDestFreeBytes>>20):
				default:
				}
				cancel()
				return
			}
		}
	}
}

// speedTracker measures average transfer speed since it was created.
type speedTracker struct {
	startTime time.Time
	bytes     int64 // accessed via atomic ops
}

func newSpeedTracker() *speedTracker {
	return &speedTracker{startTime: time.Now()}
}

func (t *speedTracker) add(n int64) {
	atomic.AddInt64(&t.bytes, n)
}

// format returns a human-readable speed string, or "" if not enough data yet.
func (t *speedTracker) format() string {
	elapsed := time.Since(t.startTime).Seconds()
	if elapsed < 0.5 {
		return ""
	}
	bps := float64(atomic.LoadInt64(&t.bytes)) / elapsed
	switch {
	case bps >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.1f KB/s", bps/(1<<10))
	default:
		return fmt.Sprintf("%.0f B/s", bps)
	}
}

// Config describes a single backup run.
type Config struct {
	TargetType  string // "nfs" or "smb" — used only when Method=="cp"
	Host        string
	Share       string
	SMBUser     string
	SMBPass     string
	SSHUser     string // used when Method=="rsync"; empty = key-based auth as current user
	SSHPass     string // used when Method=="rsync" and password auth is required; empty = key-based
	LocalPath   string
	RemotePath  string // subdirectory within the remote path; may be empty
	Direction   string // "push" or "pull"
	Method      string // "cp" or "rsync"
	Parallelism int    // used only when Method=="cp"; rsync always uses a single stream
	UseSSH      bool   // Method=="rsync" only: true=direct SSH transport, false=mount NFS/SMB and rsync locally
	Compress    bool   // rsync --compress (zstd on rsync 3.2+; zlib otherwise)
	DeleteMode  bool   // rsync --delete
}

// Run executes the backup described by cfg.
//
// For method="rsync" the remote end is reached via rsync's native SSH
// transport — no NFS or SMB mount is performed. SSH key-based auth is used
// by default; set SSHPass for password-based auth (sshpass required).
//
// For method="cp" the remote share is mounted via NFS or SMB, the tree is
// copied with sha256 verification, then the mount is torn down.
//
// progress is called with status messages and file counts (done, total) as the
// job proceeds. done and total are -1 when no count is available. Returns a
// human-readable summary on success.
func Run(ctx context.Context, cfg Config, progress func(msg string, done, total int)) (string, error) {
	if err := validateMountedLocalPath(cfg.LocalPath); err != nil {
		return "", err
	}
	switch cfg.Method {
	case "rsync":
		if cfg.UseSSH {
			return rsyncSSH(ctx, cfg, progress)
		}
		return rsyncMount(ctx, cfg, progress)
	case "cp":
		return runCP(ctx, cfg, progress)
	default:
		return "", fmt.Errorf("unknown method: %s", cfg.Method)
	}
}

func validateMountedLocalPath(localPath string) error {
	cleaned := filepath.Clean(localPath)
	if cleaned == "/mnt" || !strings.HasPrefix(cleaned, "/mnt/") {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(cleaned, "/mnt/"), string(os.PathSeparator))
	if len(parts) == 0 || parts[0] == "" {
		return nil
	}
	anchor := filepath.Join("/mnt", parts[0])

	for candidate := cleaned; strings.HasPrefix(candidate, "/mnt/"); candidate = filepath.Dir(candidate) {
		candidateDev, err := statDevice(candidate)
		if err != nil {
			parent := filepath.Dir(candidate)
			if parent == candidate || parent == "/mnt" {
				return fmt.Errorf("local path %s resolves to the root filesystem because %s is not mounted", localPath, anchor)
			}
			continue
		}
		rootDev, err := statDevice("/")
		if err != nil {
			return fmt.Errorf("stat root filesystem: %w", err)
		}
		if candidateDev == rootDev {
			return fmt.Errorf("local path %s resolves to the root filesystem because %s is not mounted", localPath, candidate)
		}
		return nil
	}
	return nil
}

func statDevice(path string) (uint64, error) {
	info, err := statPath(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: missing device metadata", path)
	}
	return uint64(st.Dev), nil
}

// rsyncSSH runs rsync directly over SSH without mounting any remote filesystem.
// This avoids per-file NFS RPC overhead and is dramatically faster for
// small-file workloads: the sender process runs on the remote host, builds
// the file list locally, and streams a single batched transfer over the
// SSH pipe rather than issuing individual LOOKUP/READ/WRITE RPCs per file.
func rsyncSSH(ctx context.Context, cfg Config, progress func(msg string, done, total int)) (string, error) {
	// Build the remote path: user@host:/share/[remotepath]/
	remoteBase := cfg.Share
	if cfg.RemotePath != "" {
		remoteBase = filepath.Join(cfg.Share, cfg.RemotePath)
	}
	if !strings.HasSuffix(remoteBase, "/") {
		remoteBase += "/"
	}
	remoteSpec := fmt.Sprintf("%s:%s", cfg.Host, remoteBase)
	if cfg.SSHUser != "" {
		remoteSpec = fmt.Sprintf("%s@%s:%s", cfg.SSHUser, cfg.Host, remoteBase)
	}

	localPath := cfg.LocalPath
	if !strings.HasSuffix(localPath, "/") {
		localPath += "/"
	}
	if err := os.MkdirAll(localPath, 0o755); err != nil {
		return "", fmt.Errorf("create local path: %w", err)
	}

	// SSH transport args. StrictHostKeyChecking=accept-new accepts first-time
	// connections automatically (safe for a controlled NAS environment) but
	// rejects changed host keys so we still detect MITM on known hosts.
	//
	// When a password is supplied: disable pubkey auth entirely so SSH goes
	// straight to password auth without failing on a missing key first.
	// sshpass reads the password from the SSHPASS env var (-e flag).
	//
	// ServerAliveInterval=60/ServerAliveCountMax=3 (3 min total): SSH-level
	// backstop only — protects against SSH itself freezing at the protocol
	// layer (request_wait_answer) when the remote end is completely unresponsive.
	// rsync's own --timeout (below) is the primary stall detector and fires
	// first; these keepalives are only reached if rsync's I/O layer hangs before
	// it can observe the data timeout.
	const sshKeepAlive = "-o ServerAliveInterval=60 -o ServerAliveCountMax=3"
	var sshArgs string
	if cfg.SSHPass != "" {
		sshArgs = "sshpass -e ssh -o StrictHostKeyChecking=accept-new -o BatchMode=no -o PubkeyAuthentication=no -o PreferredAuthentications=password " + sshKeepAlive
	} else {
		sshArgs = "ssh -o StrictHostKeyChecking=accept-new -o BatchMode=yes " + sshKeepAlive
	}

	var src, dst string
	if cfg.Direction == "pull" {
		src = remoteSpec
		dst = localPath
	} else {
		src = localPath
		dst = remoteSpec
	}

	// -aW: archive + whole-file (skip delta algorithm — pointless for new/changed
	// files when the bottleneck is per-file RPC latency, not bandwidth).
	// --inplace avoids rsync's temp-file + rename path. On smoothfs this is
	// materially faster for small-file backups because each temp file otherwise
	// pays a full create, placement, metadata, and rename cycle. New files are
	// still placed by smoothfs when rsync creates the final path.
	// --timeout=60: exit if no data moves for 60 seconds. This is the primary
	// stall detector — it fires at the rsync protocol level based on actual
	// data flow, independent of SSH keepalives. A server under heavy I/O load
	// may delay SSH protocol keepalive responses (causing false kills) but
	// cannot delay actual rsync data without triggering this timeout.
	args := rsyncArchiveArgs(dst)
	if cfg.Compress {
		// rsync 3.2+ negotiates zstd; older rsync falls back to zlib. Either
		// way this only helps when the wire is slower than available CPU —
		// for already-compressed media (mp4/mkv/zip) it's net-negative.
		args = append(args, "--compress")
	}
	if cfg.DeleteMode {
		args = append(args, "--delete")
	}
	args = append(args, "-e", sshArgs, src, dst)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reasonCh := make(chan string, 1)

	// Pre-flight + watchdog only when dst is a local path we can statfs
	// (pull). Push writes to a remote share we can't introspect.
	if cfg.Direction == "pull" {
		if free, ferr := destFreeBytes(localPath); ferr != nil {
			log.Printf("backup: statfs %s: %v (skipping pre-flight)", localPath, ferr)
		} else if free < minDestFreeBytes {
			return "", fmt.Errorf("destination %s has only %d MB free (< %d MB minimum); refusing to start", localPath, free>>20, minDestFreeBytes>>20)
		}
		go watchDestFree(runCtx, localPath, cancel, reasonCh)
	}

	cmd := exec.CommandContext(runCtx, "rsync", args...)
	if cfg.SSHPass != "" {
		cmd.Env = append(os.Environ(), "SSHPASS="+cfg.SSHPass)
	}

	// Report wire rate in addition to rsync's logical byte counter when the
	// user has asked for compression — that's the whole point of showing both.
	summary, err := runRsyncProcess(cmd, "Running rsync over SSH...", cfg.Direction, cfg.Compress, progress)
	if err != nil {
		select {
		case reason := <-reasonCh:
			return "", fmt.Errorf("%s: %w", reason, err)
		default:
			return "", err
		}
	}
	return summary, nil
}

// runRsyncProcess starts the given rsync exec.Cmd, streams its stdout while
// emitting live rate updates, waits for exit, and returns the parsed --stats
// summary. Used by both rsyncSSH and rsyncMount so progress handling stays
// identical regardless of transport.
//
// When showWire is true the emitted rate is formatted as "<wire> (<logical>)"
// — wire being the NIC delta and logical being rsync's own progress counter,
// which is pre-compression file offset. That lets users watch the compression
// ratio in real time. Only meaningful for rsync-over-SSH with --compress;
// callers pass false for the mount path where rsync can't reach the wire.
//
// No stall watchdog here: rsync's own --timeout=60 exits cleanly if no data
// moves for that long, which is the right place to detect stalls. A net-rate
// watchdog with an adaptive threshold caused false positives on mixed
// workloads where large-file phases (280 MB/s) are followed by legitimate
// small-file phases (10 MB/s).
func runRsyncProcess(cmd *exec.Cmd, startLabel, direction string, showWire bool, progress func(msg string, done, total int)) (string, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("rsync stdout pipe: %w", err)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	progress(startLabel, -1, -1)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("rsync start: %w", err)
	}

	type sample struct {
		t     time.Time
		bytes int64 // rsync progress2 byte offset (logical / post-decompression)
		wire  int64 // total NIC tx/rx bytes across non-loopback interfaces
	}
	const rateWindow = 3 * time.Second
	var samples []sample
	var statsBuf strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	scanner.Split(splitCRLF)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if b, _, ok := parseRsyncProgress(line); ok {
			now := time.Now()
			var wire int64
			if showWire {
				if w, ok := readNetBytes(direction); ok {
					wire = int64(w)
				}
			}
			samples = append(samples, sample{t: now, bytes: b, wire: wire})
			cutoff := now.Add(-rateWindow)
			for len(samples) > 1 && samples[0].t.Before(cutoff) {
				samples = samples[1:]
			}
			if len(samples) >= 2 {
				first := samples[0]
				last := samples[len(samples)-1]
				elapsed := last.t.Sub(first.t).Seconds()
				if elapsed > 0 && last.bytes >= first.bytes {
					logical := float64(last.bytes-first.bytes) / elapsed
					if showWire && last.wire >= first.wire && first.wire > 0 {
						wireRate := float64(last.wire-first.wire) / elapsed
						progress(fmt.Sprintf("rsync: %s (%s)", formatRate(wireRate), formatRate(logical)), -1, -1)
					} else {
						progress("rsync: "+formatRate(logical), -1, -1)
					}
				}
			}
		} else {
			statsBuf.WriteString(line)
			statsBuf.WriteByte('\n')
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		if waitErr := cmd.Wait(); waitErr != nil {
			return "", fmt.Errorf("rsync output scan: %w: %v", scanErr, formatRsyncError(waitErr, errBuf.String()))
		}
		return "", fmt.Errorf("rsync output scan: %w", scanErr)
	}

	if err := cmd.Wait(); err != nil {
		return "", formatRsyncError(err, errBuf.String())
	}
	summary := parsersyncSummary(statsBuf.String())
	progress("rsync complete", -1, -1)
	return summary, nil
}

func formatRsyncError(waitErr error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if strings.Contains(stderr, "No space left on device") {
		if path := parseRsyncWriteFailedPath(stderr); path != "" {
			return fmt.Errorf("rsync: destination filesystem is full at %s", path)
		}
		return fmt.Errorf("rsync: destination filesystem is full")
	}
	if stderr == "" {
		return fmt.Errorf("rsync: %w", waitErr)
	}
	return fmt.Errorf("rsync: %w: %s", waitErr, stderr)
}

func parseRsyncWriteFailedPath(stderr string) string {
	const marker = "write failed on \""
	idx := strings.Index(stderr, marker)
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	end := strings.Index(stderr[start:], "\"")
	if end < 0 {
		return ""
	}
	return stderr[start : start+end]
}

// rsyncMount mounts the remote NFS/SMB share and runs rsync locally between
// the smoothfs mount and the mount point. Preferred when rsync over SSH isn't
// available on the remote, or when the mount-based kernel NFS stack is
// faster than going through an SSH tunnel (typical on a fast LAN, where
// SSH crypto is the single-thread bottleneck).
func rsyncMount(ctx context.Context, cfg Config, progress func(msg string, done, total int)) (string, error) {
	mountDir, err := os.MkdirTemp("", "smoothnas-backup-*")
	if err != nil {
		return "", fmt.Errorf("create mount dir: %w", err)
	}
	defer os.Remove(mountDir)

	progress("Mounting remote target...", -1, -1)
	if err := mount(cfg, mountDir); err != nil {
		return "", fmt.Errorf("mount failed: %w", err)
	}
	defer umount(mountDir) //nolint:errcheck

	remoteFull := mountDir
	if cfg.RemotePath != "" {
		remoteFull = filepath.Join(mountDir, filepath.Clean("/"+cfg.RemotePath))
		if cfg.Direction == "push" {
			if err := os.MkdirAll(remoteFull, 0o755); err != nil {
				return "", fmt.Errorf("create remote destination subdir: %w", err)
			}
		} else if info, err := os.Stat(remoteFull); err != nil {
			return "", fmt.Errorf("stat remote source subdir: %w", err)
		} else if !info.IsDir() {
			return "", fmt.Errorf("remote source is not a directory: %s", remoteFull)
		}
	}

	if cfg.Direction == "pull" {
		if info, err := os.Stat(remoteFull); err != nil {
			return "", fmt.Errorf("stat remote source: %w", err)
		} else if !info.IsDir() {
			return "", fmt.Errorf("remote source is not a directory: %s", remoteFull)
		}
	}

	var src, dst string
	if cfg.Direction == "push" {
		src = cfg.LocalPath
		dst = remoteFull
	} else {
		src = remoteFull
		dst = cfg.LocalPath
	}
	if !strings.HasSuffix(src, "/") {
		src += "/"
	}
	if !strings.HasSuffix(dst, "/") {
		dst += "/"
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", fmt.Errorf("create destination: %w", err)
	}

	if free, err := destFreeBytes(dst); err != nil {
		log.Printf("backup: statfs %s: %v (skipping pre-flight)", dst, err)
	} else if free < minDestFreeBytes {
		return "", fmt.Errorf("destination %s has only %d MB free (< %d MB minimum); refusing to start", dst, free>>20, minDestFreeBytes>>20)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reasonCh := make(chan string, 1)
	go watchDestFree(runCtx, dst, cancel, reasonCh)

	// Compression is intentionally ignored on the NFS/SMB mount path:
	// rsync is copying between local paths, so it cannot compress kernel
	// RPC traffic and can only add CPU overhead on LAN transfers.
	args := rsyncMountArgs(dst)
	if cfg.DeleteMode {
		args = append(args, "--delete")
	}
	args = append(args, src, dst)
	cmd := exec.CommandContext(runCtx, "rsync", args...)
	// Mounted NFS/SMB backups copy between local paths. rsync cannot see kernel
	// RPC wire bytes here, so progress reports logical payload throughput.
	summary, err := runRsyncProcess(cmd, fmt.Sprintf("Running rsync over %s mount...", strings.ToUpper(cfg.TargetType)), cfg.Direction, false, progress)
	if err != nil {
		select {
		case reason := <-reasonCh:
			return "", fmt.Errorf("%s: %w", reason, err)
		default:
			return "", err
		}
	}
	return summary, nil
}

// runCP mounts the remote NFS or SMB share, copies the tree with sha256
// verification, then unmounts.
func runCP(ctx context.Context, cfg Config, progress func(msg string, done, total int)) (string, error) {
	mountDir, err := os.MkdirTemp("", "smoothnas-backup-*")
	if err != nil {
		return "", fmt.Errorf("create mount dir: %w", err)
	}
	defer os.Remove(mountDir)

	progress("Mounting remote target...", -1, -1)
	if err := mount(cfg, mountDir); err != nil {
		return "", fmt.Errorf("mount failed: %w", err)
	}
	defer umount(mountDir) //nolint:errcheck

	remoteFull := mountDir
	if cfg.RemotePath != "" {
		remoteFull = filepath.Join(mountDir, filepath.Clean("/"+cfg.RemotePath))
		if cfg.Direction == "push" {
			if err := os.MkdirAll(remoteFull, 0o755); err != nil {
				return "", fmt.Errorf("create remote destination subdir: %w", err)
			}
		} else if info, err := os.Stat(remoteFull); err != nil {
			return "", fmt.Errorf("stat remote source subdir: %w", err)
		} else if !info.IsDir() {
			return "", fmt.Errorf("remote source is not a directory: %s", remoteFull)
		}
	}

	if cfg.Direction == "pull" {
		if info, err := os.Stat(remoteFull); err != nil {
			return "", fmt.Errorf("stat remote source: %w", err)
		} else if !info.IsDir() {
			return "", fmt.Errorf("remote source is not a directory: %s", remoteFull)
		}
	}

	var src, dst string
	if cfg.Direction == "push" {
		src = cfg.LocalPath
		dst = remoteFull
	} else {
		src = remoteFull
		dst = cfg.LocalPath
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", fmt.Errorf("create destination: %w", err)
	}

	if free, ferr := destFreeBytes(dst); ferr != nil {
		log.Printf("backup: statfs %s: %v (skipping pre-flight)", dst, ferr)
	} else if free < minDestFreeBytes {
		return "", fmt.Errorf("destination %s has only %d MB free (< %d MB minimum); refusing to start", dst, free>>20, minDestFreeBytes>>20)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reasonCh := make(chan string, 1)
	go watchDestFree(runCtx, dst, cancel, reasonCh)

	count, err := cpWithHash(runCtx, src, dst, progress)
	if err != nil {
		select {
		case reason := <-reasonCh:
			return "", fmt.Errorf("%s: %w", reason, err)
		default:
			return "", err
		}
	}
	return fmt.Sprintf("cp backup complete — %d files verified", count), nil
}

// mount mounts an NFS or SMB share at mountDir.
func mount(cfg Config, mountDir string) error {
	var cmd *exec.Cmd
	switch cfg.TargetType {
	case "nfs":
		cmd = exec.Command("mount", "-t", "nfs", "-o", nfsMountOpts,
			fmt.Sprintf("%s:%s", cfg.Host, cfg.Share),
			mountDir,
		)
	case "smb":
		opts := fmt.Sprintf("user=%s,password=%s,vers=3.0", cfg.SMBUser, cfg.SMBPass)
		cmd = exec.Command("mount", "-t", "cifs",
			fmt.Sprintf("//%s/%s", cfg.Host, cfg.Share),
			mountDir, "-o", opts,
		)
	default:
		return fmt.Errorf("unknown target type: %s", cfg.TargetType)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	if cfg.TargetType == "nfs" {
		if err := setBDIReadAhead(mountDir, nfsReadAheadKB); err != nil {
			log.Printf("backup: nfs readahead tune for %s: %v", mountDir, err)
		}
	}
	return nil
}

// setBDIReadAhead writes kb to the per-mount Backing Device Info readahead.
// For NFS the BDI is keyed by major:minor of the mount's device id; we get
// that via stat() then write to /sys/class/bdi/<maj>:<min>/read_ahead_kb.
func setBDIReadAhead(mountDir string, kb int) error {
	var st syscall.Stat_t
	if err := syscall.Stat(mountDir, &st); err != nil {
		return fmt.Errorf("stat %s: %w", mountDir, err)
	}
	major := unix.Major(st.Dev)
	minor := unix.Minor(st.Dev)
	path := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", major, minor)
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", kb)), 0o644)
}

func umount(mountDir string) error {
	// -l (lazy) detaches the mount from the filesystem namespace immediately
	// even if a child process (e.g. rsync) still holds a file descriptor open.
	// Without -l, umount returns EBUSY and the mount leaks across tierd restarts.
	out, err := exec.Command("umount", "-l", mountDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CleanupOrphanedMounts tears down any /tmp/smoothnas-backup-* mounts and
// directories left behind by a previous tierd instance.
//
// rsyncMount and runCP use `defer umount` for the normal exit path, but
// defers do not run when tierd is killed (SIGKILL, OOM, panic, or a
// systemd stop that races shutdown). Each interrupted backup leaks an
// NFS/SMB mount into the host namespace; over a series of restarts this
// accumulates kernel NFS state and leaves stale mount entries in /proc/mounts.
//
// Called once at startup before any new backup can begin.
func CleanupOrphanedMounts() {
	matches, err := filepath.Glob("/tmp/smoothnas-backup-*")
	if err != nil {
		log.Printf("backup: cleanup glob: %v", err)
		return
	}
	mounts, err := readMountedPaths()
	if err != nil {
		log.Printf("backup: cleanup read mounts: %v", err)
		// Fall through — still attempt rmdir on the dirs we can see.
	}
	for _, dir := range matches {
		if mounts[dir] {
			if err := umount(dir); err != nil {
				log.Printf("backup: cleanup umount %s: %v", dir, err)
				continue
			}
			log.Printf("backup: cleanup unmounted orphan %s", dir)
		}
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			log.Printf("backup: cleanup remove %s: %v", dir, err)
		}
	}
}

// readMountedPaths returns the set of mount points currently in /proc/mounts.
func readMountedPaths() (map[string]bool, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			out[fields[1]] = true
		}
	}
	return out, scanner.Err()
}

// cpWithHash recursively copies src into dst and verifies sha256 checksums.
//
// Phase 1 — scan: walk the source tree (NFS reads only) and collect
// destination directory paths and file pairs. No destination I/O happens
// here, so the walk is limited only by NFS read latency.
//
// Phase 2 — mkdir: create all destination directories in parallel using
// dirWorkers goroutines. os.MkdirAll is safe to call concurrently on
// overlapping paths: if goroutine B needs parent "a/b" before goroutine A
// has finished creating it, MkdirAll recurses upward and creates the parent
// itself; the racing EEXIST from the winner is silently absorbed.
//
// Phase 3 — copy: copy and sha256-verify files in parallel using
// copyWorkers goroutines. Files are only started after all directories
// exist so no worker ever hits a missing parent.
func cpWithHash(ctx context.Context, src, dst string, progress func(msg string, done, total int)) (int, error) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return 0, fmt.Errorf("stat source: %w", err)
	}
	if !srcInfo.IsDir() {
		return 0, fmt.Errorf("source must be a directory: %s", src)
	}

	type fileWork struct{ srcPath, dstPath, rel string }
	var files []fileWork
	var dstDirs []string

	// Phase 1: scan source tree. No destination I/O — just collect paths.
	progress("Scanning source...", -1, -1)
	if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			dstDirs = append(dstDirs, target)
			return nil
		}
		files = append(files, fileWork{path, target, rel})
		return nil
	}); err != nil {
		return 0, err
	}

	// Phase 2: create destination directories in parallel.
	{
		var (
			mu       sync.Mutex
			nDone    int
			firstErr error
		)
		sem := make(chan struct{}, dirWorkers)
		var wg sync.WaitGroup
		nDirs := len(dstDirs)
		progress(fmt.Sprintf("Creating directories (0/%d)...", nDirs), -1, -1)

		for _, d := range dstDirs {
			if ctx.Err() != nil {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(d string) {
				defer func() { <-sem; wg.Done() }()
				if err := os.MkdirAll(d, 0o755); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				nDone++
				progress(fmt.Sprintf("Creating directories (%d/%d)...", nDone, nDirs), -1, -1)
				mu.Unlock()
			}(d)
		}
		wg.Wait()

		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if firstErr != nil {
			return 0, firstErr
		}
	}

	// Phase 3: copy and verify files in parallel.
	total := len(files)
	speed := newSpeedTracker()

	var (
		mu       sync.Mutex
		count    int
		firstErr error
	)

	sem := make(chan struct{}, copyWorkers)
	var wg sync.WaitGroup

	for _, fw := range files {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(fw fileWork) {
			defer func() { <-sem; wg.Done() }()
			if ctx.Err() != nil {
				return
			}
			n, err := copyAndVerify(fw.srcPath, fw.dstPath)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", fw.rel, err)
				}
				return
			}
			speed.add(n)
			count++
			msg := fmt.Sprintf("Copied: %s", fw.rel)
			if s := speed.format(); s != "" {
				msg += " — " + s
			}
			progress(msg, count, total)
		}(fw)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return count, ctx.Err()
	}
	return count, firstErr
}

// copyAndVerify copies src to dst and verifies integrity with sha256.
// The destination is opened O_RDWR so it can be read back after writing
// using the same file descriptor — avoiding any re-lookup through the
// smoothfs dir cache, which can race with background DIR_UPDATE scans.
// Returns the number of bytes written.
func copyAndVerify(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat source: %w", err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, info.Mode())
	if err != nil {
		return 0, fmt.Errorf("open destination: %w", err)
	}
	defer out.Close()

	// Large buffer reduces syscall and NFS RPC overhead on fast links.
	buf := make([]byte, copyBufSize)

	// Hash source while copying so we only read it once.
	srcHash := sha256.New()
	n, err := io.CopyBuffer(out, io.TeeReader(in, srcHash), buf)
	if err != nil {
		return n, fmt.Errorf("write: %w", err)
	}
	if err := out.Sync(); err != nil {
		return n, fmt.Errorf("sync: %w", err)
	}

	// Hash destination from the already-open fd to avoid a smoothfs path
	// lookup. Seek back to the start and read from the same descriptor.
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return n, fmt.Errorf("seek destination: %w", err)
	}
	dstHash := sha256.New()
	if _, err := io.CopyBuffer(dstHash, out, buf); err != nil {
		return n, fmt.Errorf("read destination: %w", err)
	}

	if !bytes.Equal(srcHash.Sum(nil), dstHash.Sum(nil)) {
		return n, fmt.Errorf("sha256 mismatch")
	}
	return n, nil
}

// readNetBytes returns the cumulative byte counter across all non-loopback
// interfaces from /proc/net/dev. direction picks rx (pull) vs tx (push).
//
// /proc/net/dev format (after splitting "iface:" → strings.Fields):
//
//	[0]=rx_bytes  [1]=rx_packets [2]=rx_errs [3]=rx_drop
//	[4]=rx_fifo   [5]=rx_frame   [6]=rx_compressed [7]=rx_multicast
//	[8]=tx_bytes  [9]=tx_packets ...
func readNetBytes(direction string) (uint64, bool) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	var total uint64
	colIdx := 0 // rx_bytes by default
	if direction == "push" {
		colIdx = 8 // tx_bytes
	}
	for scan.Scan() {
		line := scan.Text()
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue // header line
		}
		name := strings.TrimSpace(line[:colon])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) <= colIdx {
			continue
		}
		v, err := strconv.ParseUint(fields[colIdx], 10, 64)
		if err != nil {
			continue
		}
		total += v
	}
	return total, true
}

// readWchan returns the kernel symbol the process is currently blocked on,
// or "" if the file can't be read (process gone, permissions).
func readWchan(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/wchan", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// splitCRLF is a bufio.SplitFunc that splits on \n or \r.
func splitCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// parseRsyncProgress parses an --info=progress2 line and extracts the
// cumulative bytes transferred and rsync's own reported speed.
// Returns (bytes, speed, true) on match.
// Example line: "     1,234,567  45%   12.34MB/s    0:00:03 (xfr#2, to-chk=5/10)"
func parseRsyncProgress(line string) (int64, string, bool) {
	if !strings.Contains(line, "xfr#") {
		return 0, "", false
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 0, "", false
	}
	bytes, err := strconv.ParseInt(strings.ReplaceAll(fields[0], ",", ""), 10, 64)
	if err != nil {
		return 0, "", false
	}
	speed := ""
	for _, f := range fields {
		if strings.HasSuffix(f, "B/s") {
			speed = f
			break
		}
	}
	return bytes, speed, true
}

// formatRate renders a bytes-per-second rate in the same style as rsync's
// own speed column, so the UI sees a consistent format.
func formatRate(bps float64) string {
	switch {
	case bps >= 1<<30:
		return fmt.Sprintf("%.2fGB/s", bps/(1<<30))
	case bps >= 1<<20:
		return fmt.Sprintf("%.2fMB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.2fKB/s", bps/(1<<10))
	default:
		return fmt.Sprintf("%.0fB/s", bps)
	}
}

// parsersyncSummary extracts the "Number of files transferred" and
// "Total transferred file size" lines from rsync --stats output.
func parsersyncSummary(output string) string {
	var transferred, size string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Number of regular files transferred:") {
			transferred = line
		}
		if strings.HasPrefix(line, "Total transferred file size:") {
			size = line
		}
	}
	if transferred != "" && size != "" {
		return fmt.Sprintf("%s; %s", transferred, size)
	}
	return "rsync completed"
}
