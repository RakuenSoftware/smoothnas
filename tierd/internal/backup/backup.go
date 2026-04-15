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

	"golang.org/x/sys/unix"
)

const (
	// copyBufSize is the per-goroutine buffer for io.CopyBuffer. 4 MB amortises
	// NFS RPC and FUSE write overhead over large chunks, giving close to wire speed
	// on fast links without meaningful extra memory cost.
	copyBufSize = 4 * 1024 * 1024

	// copyWorkers is the number of files copied in parallel. Parallel copies hide
	// per-file RPC round-trip latency when the directory tree has many small files,
	// and keep the NFS link and local disk both busy simultaneously.
	copyWorkers = 4

	// dirWorkers is the number of directories created in parallel. os.MkdirAll
	// is safe to call concurrently on overlapping paths: racing goroutines that
	// need the same parent will create it themselves and the EEXIST from the
	// winner is silently absorbed. More workers hide the FUSE/tierd round-trip
	// latency that makes sequential mkdir the dominant bottleneck on large trees.
	dirWorkers = 16

	// nfsMountOpts are appended to every NFS mount so the kernel uses large
	// RPC read/write payloads and opens multiple parallel TCP connections to the
	// server. rsize/wsize=1M amortises NFS RPC overhead over large chunks.
	// nconnect=8 opens 8 TCP sessions to the server; a single TCP stream is
	// often the binding constraint on NFS read throughput because the congestion
	// window can only grow to fill one pipe — parallel sessions let the kernel
	// spread RPCs across multiple flows and approach line rate.
	// vers=4.2 pins the protocol to NFSv4. NFSv3 needs rpc.statd for NLM
	// locking, which on a fresh boot is not yet running; mount.nfs starts it
	// lazily, but the first mount can race the server and get "access denied"
	// before statd has registered. NFSv4 uses in-protocol locking and avoids
	// the race entirely.
	//
	// rsize/wsize=2M is the conservative bump we land on. 1M was original
	// tested-good; 4M regressed against one real NFS server. 2M is a
	// compromise that helps large-file throughput without the 4M risk —
	// retest if perf changes show up.
	//
	// lookupcache=all + actimeo=60 cache directory entries and file attrs
	// for up to a minute. During rsync's tree walk this turns repeated
	// stat calls on the same parent dirs into local lookups instead of
	// NFS RPCs. Initial backups gain modestly (each file still stat'd
	// once); incremental rsync runs gain a lot.
	nfsMountOpts = "vers=4.2,rsize=2097152,wsize=2097152,nconnect=8,lookupcache=all,actimeo=60"

	// nfsReadAheadKB raises the per-mount BDI readahead so the kernel
	// pre-fetches further into a sequential NFS read stream. 4 MB is a
	// modest bump from the kernel default (~128 KB) — enough to keep the
	// server's prefetcher warm without risking memory pressure under load.
	nfsReadAheadKB = 4096
)

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
	TargetType  string // "nfs" or "smb"
	Host        string
	Share       string
	SMBUser     string
	SMBPass     string
	LocalPath   string
	RemotePath  string // subdirectory within the mounted share; may be empty
	Direction   string // "push" or "pull"
	Method      string // "cp" or "rsync"
	Parallelism int    // number of concurrent rsyncs (1 = single stream)
}

// Run mounts the remote target, executes the backup, and unmounts.
// progress is called with status messages and file counts (done, total) as the
// job proceeds. done and total are -1 when no count is available (e.g. rsync).
// Returns a human-readable summary on success.
func Run(ctx context.Context, cfg Config, progress func(msg string, done, total int)) (string, error) {
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

	// Resolve the remote subpath within the mount.
	remoteFull := mountDir
	if cfg.RemotePath != "" {
		remoteFull = filepath.Join(mountDir, filepath.Clean("/"+cfg.RemotePath))
		if err := os.MkdirAll(remoteFull, 0o755); err != nil {
			return "", fmt.Errorf("create remote subdir: %w", err)
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

	switch cfg.Method {
	case "cp":
		count, err := cpWithHash(ctx, src, dst, progress)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("cp backup complete — %d files verified", count), nil
	case "rsync":
		progress("Running rsync...", -1, -1)
		var summary string
		var err error
		if cfg.Parallelism > 1 {
			summary, err = rsyncBackupParallel(ctx, src, dst, cfg.Direction, cfg.Parallelism, progress)
		} else {
			summary, err = rsyncBackup(ctx, src, dst, cfg.Direction, progress)
		}
		if err != nil {
			return "", err
		}
		return summary, nil
	default:
		return "", fmt.Errorf("unknown method: %s", cfg.Method)
	}
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
// FUSE dir cache, which can race with background DIR_UPDATE scans.
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

	// Hash destination from the already-open fd to avoid a FUSE path
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

// rsyncBackupParallel splits the source's top-level entries round-robin
// across N concurrent rsync processes. Each rsync writes to the same dst
// (with --relative the source path stays in the right place under dst).
// Per-stream rates are aggregated into a single live progress value so
// the UI sees a single "rsync: NN MB/s (parallel × N)" line.
//
// Source-side stalls on one stream don't block the others, so aggregate
// throughput stays much smoother than a single rsync would.
func rsyncBackupParallel(ctx context.Context, src, dst, direction string, n int, progress func(msg string, done, total int)) (string, error) {
	if n < 2 {
		return rsyncBackup(ctx, src, dst, direction, progress)
	}
	if !strings.HasSuffix(src, "/") {
		src += "/"
	}
	if !strings.HasSuffix(dst, "/") {
		dst += "/"
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return "", fmt.Errorf("read source dir for parallel split: %w", err)
	}
	if len(entries) == 0 {
		return "rsync: nothing to copy", nil
	}
	// Round-robin partition by entry name.
	buckets := make([][]string, n)
	for i, e := range entries {
		buckets[i%n] = append(buckets[i%n], e.Name())
	}
	// Aggregate rate state shared across streams.
	var (
		aggMu        sync.Mutex
		streamRates  = make([]float64, n)
		streamFinished = make([]bool, n)
		lastEmit     time.Time
	)
	emitAggregate := func() {
		aggMu.Lock()
		var total float64
		active := 0
		for i, r := range streamRates {
			total += r
			if !streamFinished[i] {
				active++
			}
		}
		now := time.Now()
		if now.Sub(lastEmit) >= 500*time.Millisecond {
			lastEmit = now
			aggMu.Unlock()
			progress(fmt.Sprintf("rsync: %s (parallel × %d, %d active)",
				formatRate(total), n, active), -1, -1)
			return
		}
		aggMu.Unlock()
	}

	wg := sync.WaitGroup{}
	errs := make(chan error, n)
	summaries := make([]string, n)

	streamCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	for i := 0; i < n; i++ {
		if len(buckets[i]) == 0 {
			streamFinished[i] = true
			continue
		}
		wg.Add(1)
		go func(idx int, names []string) {
			defer wg.Done()
			defer func() {
				aggMu.Lock()
				streamFinished[idx] = true
				streamRates[idx] = 0
				aggMu.Unlock()
				emitAggregate()
			}()
			perStream := func(msg string, _, _ int) {
				if !strings.HasPrefix(msg, "rsync: ") {
					return
				}
				rate := parseRateBytes(strings.TrimPrefix(msg, "rsync: "))
				aggMu.Lock()
				streamRates[idx] = rate
				aggMu.Unlock()
				emitAggregate()
			}
			summary, serr := rsyncBackupSubset(streamCtx, src, dst, direction, names, perStream)
			summaries[idx] = summary
			if serr != nil {
				errs <- fmt.Errorf("stream %d: %w", idx, serr)
				cancelAll()
			}
		}(i, buckets[i])
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			return "", e
		}
	}
	progress("rsync complete", -1, -1)
	return fmt.Sprintf("parallel rsync: %d streams completed", n), nil
}

// rsyncBackupSubset is rsyncBackup that only transfers the named
// top-level entries of src into dst. Same rate-tracking + watchdog as
// rsyncBackup, but reports rate via the supplied progress callback so the
// parallel coordinator can aggregate.
func rsyncBackupSubset(ctx context.Context, src, dst, direction string, names []string, progress func(msg string, done, total int)) (string, error) {
	// -W (--whole-file) skips the delta algorithm — pointless for new files
	// (no dest to delta against) and the per-block hashing is heavy overhead
	// for tiny files. --inplace writes straight to dest filename instead of
	// the temp-then-rename dance, saving one fs op per file.
	args := []string{"-aW", "--inplace", "--stats", "--no-human-readable", "--info=progress2", "--relative"}
	// Use ./ root so --relative preserves only the names, not the full src path.
	for _, name := range names {
		args = append(args, src+"./"+name)
	}
	args = append(args, dst)

	cmd := exec.CommandContext(ctx, "rsync", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("rsync stdout pipe: %w", err)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("rsync start: %w", err)
	}

	state := &emitState{lastEmit: time.Now()}
	wgCtx, cancelWG := context.WithCancel(ctx)
	defer cancelWG()
	go netRateWatchdog(wgCtx, direction, cmd.Process.Pid, state, progress)

	type sample struct {
		t     time.Time
		bytes int64
	}
	const rateWindow = 3 * time.Second
	var samples []sample
	var statsBuf strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Split(splitCRLF)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if b, _, ok := parseRsyncProgress(line); ok {
			now := time.Now()
			samples = append(samples, sample{t: now, bytes: b})
			cutoff := now.Add(-rateWindow)
			for len(samples) > 1 && samples[0].t.Before(cutoff) {
				samples = samples[1:]
			}
			if len(samples) >= 2 {
				first := samples[0]
				last := samples[len(samples)-1]
				elapsed := last.t.Sub(first.t).Seconds()
				if elapsed > 0 && last.bytes >= first.bytes {
					rate := float64(last.bytes-first.bytes) / elapsed
					progress("rsync: "+formatRate(rate), -1, -1)
					state.mu.Lock()
					state.lastEmit = now
					state.lastBytes = b
					state.mu.Unlock()
				}
			}
		} else {
			statsBuf.WriteString(line)
			statsBuf.WriteByte('\n')
		}
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("rsync: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return parsersyncSummary(statsBuf.String()), nil
}

// parseRateBytes inverts formatRate — extracts a bytes/sec float from a
// "12.3MB/s" / "456KB/s" / "789B/s" string. Returns 0 on parse failure
// (which means the aggregator just stops counting that stream until its
// next sample, which is fine).
func parseRateBytes(s string) float64 {
	s = strings.TrimSpace(s)
	// Strip suffixes the watchdog may add, e.g. " (disk)".
	if i := strings.Index(s, " "); i > 0 {
		s = s[:i]
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "GB/s"):
		mult = 1 << 30
		s = strings.TrimSuffix(s, "GB/s")
	case strings.HasSuffix(s, "MB/s"):
		mult = 1 << 20
		s = strings.TrimSuffix(s, "MB/s")
	case strings.HasSuffix(s, "KB/s"):
		mult = 1 << 10
		s = strings.TrimSuffix(s, "KB/s")
	case strings.HasSuffix(s, "B/s"):
		s = strings.TrimSuffix(s, "B/s")
	default:
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v * mult
}

// rsyncBackup runs rsync from src/ to dst/ and returns a brief summary.
// Uses --info=progress2 for byte-count progress and supplements it with a
// disk-write watchdog so the displayed rate stays accurate even when
// rsync goes quiet for several seconds mid-large-file (which it does).
func rsyncBackup(ctx context.Context, src, dst, direction string, progress func(msg string, done, total int)) (string, error) {
	// Ensure trailing slash so rsync copies contents, not the directory itself.
	if !strings.HasSuffix(src, "/") {
		src += "/"
	}
	if !strings.HasSuffix(dst, "/") {
		dst += "/"
	}

	// -W + --inplace match the parallel-subset variant — see comment there
	// for rationale (skip delta, skip temp-rename for small files).
	cmd := exec.CommandContext(ctx, "rsync", "-aW", "--inplace", "--stats", "--no-human-readable",
		"--info=progress2", src, dst)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("rsync stdout pipe: %w", err)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("rsync start: %w", err)
	}

	// Shared progress state. The reader updates lastEmit on every emit;
	// the watchdog reads it to decide if rsync has gone quiet, and writes
	// to it when it emits a synthetic update.
	state := &emitState{lastEmit: time.Now()}

	wgCtx, cancelWG := context.WithCancel(ctx)
	defer cancelWG()
	go netRateWatchdog(wgCtx, direction, cmd.Process.Pid, state, progress)

	// Stream stdout line by line. Compute instantaneous rate over a rolling
	// 3-second window of byte-count samples so dirty-page writeback bursts
	// (which can spike disk writes to 300+ MB/s between idle periods) don't
	// show up as wild swings in the UI.
	type sample struct {
		t     time.Time
		bytes int64
	}
	const rateWindow = 3 * time.Second
	const rateMinSamples = 2
	var samples []sample

	var statsBuf strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Split(splitCRLF)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if b, _, ok := parseRsyncProgress(line); ok {
			now := time.Now()
			samples = append(samples, sample{t: now, bytes: b})
			cutoff := now.Add(-rateWindow)
			for len(samples) > 1 && samples[0].t.Before(cutoff) {
				samples = samples[1:]
			}
			if len(samples) >= rateMinSamples {
				first := samples[0]
				last := samples[len(samples)-1]
				elapsed := last.t.Sub(first.t).Seconds()
				if elapsed > 0 && last.bytes >= first.bytes {
					rate := float64(last.bytes-first.bytes) / elapsed
					progress("rsync: "+formatRate(rate), -1, -1)
					state.mu.Lock()
					state.lastEmit = now
					state.lastBytes = b
					state.mu.Unlock()
				}
			}
		} else {
			statsBuf.WriteString(line)
			statsBuf.WriteByte('\n')
		}
	}

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("rsync: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}

	summary := parsersyncSummary(statsBuf.String())
	progress("rsync complete", -1, -1)
	return summary, nil
}

// emitState carries the rsync stdout reader's "last emitted progress"
// timestamp + bytes so the disk-rate watchdog can detect quiet periods
// and supplement them with synthetic updates.
type emitState struct {
	mu        sync.Mutex
	lastEmit  time.Time
	lastBytes int64
}

// netRateWatchdog samples /proc/net/dev every 1s and emits the network
// throughput as the user-facing rate. Network is the ground truth: in a
// pull backup, all data comes in via net rx; in a push, all data goes out
// via tx. rsync's --info=progress2 byte counter lags actual throughput
// (it reports based on internal bookkeeping, not what the kernel is
// actually moving), so we ignore it for rate display and rely on net.
//
// Emits unconditionally every tick — no rsync-quiet gate, no fall-through
// from a rsync-emitted rate. The watchdog is the sole source of "how fast
// is data flowing" for the UI.
func netRateWatchdog(ctx context.Context, direction string, rsyncPID int, state *emitState, progress func(msg string, done, total int)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	prev, ok := readNetBytes(direction)
	if !ok {
		return
	}
	prevTime := time.Now()
	loggedWchan := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		now := time.Now()
		cur, ok := readNetBytes(direction)
		if !ok {
			continue
		}
		elapsed := now.Sub(prevTime).Seconds()
		var rate float64
		if elapsed > 0 && cur >= prev {
			rate = float64(cur-prev) / elapsed
		}
		prev = cur
		prevTime = now

		// Always emit the net-rate. This is the ground truth: rsync's
		// own --info=progress2 byte counter lags actual transfer.
		progress("rsync: "+formatRate(rate), -1, -1)
		state.mu.Lock()
		state.lastEmit = now
		state.mu.Unlock()

		// When the rate drops near zero, log rsync's wchan to tell us
		// what it's blocked on. Throttle to once per 10s so we don't
		// spam during sustained stalls.
		if rate < 1<<20 && time.Since(loggedWchan) > 10*time.Second {
			if w := readWchan(rsyncPID); w != "" {
				log.Printf("backup: rate low (%s), rsync wchan=%s",
					formatRate(rate), w)
			}
			loggedWchan = now
		}
	}
}

// readNetBytes returns the cumulative byte counter across all non-loopback
// interfaces from /proc/net/dev. direction picks rx (pull) vs tx (push).
//
// /proc/net/dev format (after splitting "iface:" → strings.Fields):
//   [0]=rx_bytes  [1]=rx_packets [2]=rx_errs [3]=rx_drop
//   [4]=rx_fifo   [5]=rx_frame   [6]=rx_compressed [7]=rx_multicast
//   [8]=tx_bytes  [9]=tx_packets ...
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
