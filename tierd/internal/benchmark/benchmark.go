// Package benchmark runs fio-based I/O benchmarks for SMB, NFS, and iSCSI targets.
package benchmark

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/nfs"
)

// Request holds benchmark parameters supplied by the caller.
type Request struct {
	Protocol  string `json:"protocol"`   // smb, nfs, iscsi, local
	Path      string `json:"path"`       // local filesystem path or block device (when not remote)
	DurationS int    `json:"duration"`   // seconds, 5–300
	SizeMB    int    `json:"size_mb"`    // test file size MB, 64–262144
	BlockSize string `json:"block_size"` // e.g. "4k", "128k", "1m"
	Mode      string `json:"mode"`       // randrw, randread, randwrite, read, write

	// Remote target fields. When Remote is true, the backend mounts the share,
	// runs fio against the mount point, and unmounts on completion.
	Remote       bool   `json:"remote"`
	RemoteHost   string `json:"remote_host"`   // hostname or IP
	RemoteShare  string `json:"remote_share"`  // SMB: share name; NFS: export path (must start with /)
	RemoteUser   string `json:"remote_user"`   // SMB only, optional (omit for guest)
	RemotePass   string `json:"remote_pass"`   // SMB only, optional
	MountOptions string `json:"mount_options"` // extra mount -o options; default applied if empty
}

// DataPoint is a single performance sample from a running benchmark (1-second average).
type DataPoint struct {
	ElapsedS  int     `json:"elapsed_s"`
	ReadMBPS  float64 `json:"read_mbps"`
	WriteMBPS float64 `json:"write_mbps"`
	ReadIOPS  float64 `json:"read_iops"`
	WriteIOPS float64 `json:"write_iops"`
}

// Result holds parsed fio output.
type Result struct {
	Protocol     string      `json:"protocol"`
	Path         string      `json:"path"`
	RemoteTarget string      `json:"remote_target,omitempty"` // set when Remote was true
	ReadIOPS     float64     `json:"read_iops"`
	WriteIOPS    float64     `json:"write_iops"`
	ReadBWMB     float64     `json:"read_mbps"`
	WriteBWMB    float64     `json:"write_mbps"`
	ReadLatUS    float64     `json:"read_lat_us"`
	WriteLatUS   float64     `json:"write_lat_us"`
	DurationS    int         `json:"duration_sec"`
	Mode         string      `json:"mode"`
	BlockSize    string      `json:"block_size"`
	DataPoints   []DataPoint `json:"data_points,omitempty"`
}

var validBlockSizes = map[string]bool{
	"4k": true, "8k": true, "16k": true, "32k": true,
	"64k": true, "128k": true, "512k": true, "1m": true,
}

var validModes = map[string]bool{
	"randrw": true, "randread": true, "randwrite": true,
	"read": true, "write": true,
}

var validProtocols = map[string]bool{
	"smb": true, "nfs": true, "iscsi": true, "local": true,
}

var safePath = regexp.MustCompile(`^/[a-zA-Z0-9_./ -]+$`)
var safeHost = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Validate checks request fields and returns a human-readable error or nil.
func (r *Request) Validate() error {
	if !validProtocols[r.Protocol] {
		return fmt.Errorf("protocol must be smb, nfs, iscsi, or local")
	}
	if r.DurationS < 5 || r.DurationS > 300 {
		return fmt.Errorf("duration must be between 5 and 300 seconds")
	}
	if r.SizeMB < 64 || r.SizeMB > 262144 {
		return fmt.Errorf("size_mb must be between 64 and 262144")
	}
	if !validBlockSizes[r.BlockSize] {
		return fmt.Errorf("block_size must be one of: 4k 8k 16k 32k 64k 128k 512k 1m")
	}
	if !validModes[r.Mode] {
		return fmt.Errorf("mode must be one of: randrw randread randwrite read write")
	}

	if r.Remote {
		if r.Protocol == "iscsi" || r.Protocol == "local" {
			return fmt.Errorf("remote %s benchmarking is not supported", r.Protocol)
		}
		if !safeHost.MatchString(r.RemoteHost) {
			return fmt.Errorf("remote_host must be a valid hostname or IP address")
		}
		if r.RemoteShare == "" {
			return fmt.Errorf("remote_share is required")
		}
		if r.Protocol == "nfs" && !strings.HasPrefix(r.RemoteShare, "/") {
			return fmt.Errorf("NFS remote_share must be an absolute path (e.g. /export/data)")
		}
	} else {
		if r.Path == "" || !safePath.MatchString(r.Path) {
			return fmt.Errorf("path must be an absolute filesystem path")
		}
		if r.Protocol == "iscsi" && !strings.HasPrefix(r.Path, "/dev/") {
			return fmt.Errorf("iSCSI path must be a block device under /dev/")
		}
	}
	return nil
}

// Run executes fio with the given parameters and returns parsed results.
// progressFn is called every second with an elapsed-time string; it may be nil.
// resultFn is called every second with live DataPoints during the run; it may be nil.
func Run(req Request, progressFn func(string), resultFn func(*Result)) (*Result, error) {
	benchPath := req.Path
	remoteTarget := ""

	if req.Remote {
		var remoteAddr string
		if req.Protocol == "smb" {
			remoteAddr = fmt.Sprintf("//%s/%s", req.RemoteHost, req.RemoteShare)
		} else {
			remoteAddr = fmt.Sprintf("%s:%s", req.RemoteHost, req.RemoteShare)
		}
		if progressFn != nil {
			progressFn(fmt.Sprintf("Mounting %s...", remoteAddr))
		}
		mountPoint, cleanup, err := mountRemote(req)
		if err != nil {
			return nil, fmt.Errorf("failed to mount %s: %w", remoteAddr, err)
		}
		defer cleanup()
		benchPath = mountPoint
		remoteTarget = remoteAddr
	}

	// smoothfs tier mounts (/mnt/<pool>) stack over per-tier backing stores
	// at /mnt/.tierd-backing/<pool>/<tier>/. Redirect the benchmark to the
	// first backing so fio talks to the real underlying XFS directly; the
	// smoothfs passthrough adds negligible overhead, so the numbers still
	// reflect real tier performance.
	if req.Protocol == "local" && !req.Remote && isSmoothfsMount(benchPath) {
		backing, err := smoothfsBacking(benchPath)
		if err != nil {
			return nil, fmt.Errorf("smoothfs tier mount %s: %w", benchPath, err)
		}
		if progressFn != nil {
			progressFn(fmt.Sprintf("Redirecting to backing store %s...", backing))
		}
		benchPath = backing
	}

	// Decide whether benchPath should be used directly (block device or
	// existing regular file) or treated as a directory we drop a temp file
	// into. iSCSI is always a raw block device. For "local" the user can
	// pass either a mount point (directory) or a block device such as
	// /dev/sdX — joining ".tierd_bench_tmp" onto a non-directory path
	// makes fio fail with "fstat: Not a directory" (ENOTDIR), so stat
	// the path and only append the temp file name when it is a directory.
	isBlock := req.Protocol == "iscsi"
	if !isBlock && req.Protocol == "local" {
		if fi, err := os.Stat(benchPath); err == nil && !fi.IsDir() {
			isBlock = true
		}
	}
	filename := benchPath
	if !isBlock {
		testFile := filepath.Join(benchPath, ".tierd_bench_tmp")
		filename = testFile
		defer os.Remove(testFile)
	}

	// iSCSI (raw block device) and local block-backed filesystems (ext4, xfs, …)
	// both support O_DIRECT and need a higher queue depth to saturate NVMe/Optane
	// devices.  ZFS manages its own cache via ARC and does not support O_DIRECT
	// reliably across all OpenZFS versions, so keep buffered sync I/O for ZFS.
	// smoothfs forwards I/O to its lower filesystem; libaio+O_DIRECT support
	// depends on the lower, so stick with sync engine on smoothfs paths too.
	// Network filesystems (SMB/NFS) never use O_DIRECT.
	useDirectIO := req.Protocol == "iscsi" || (req.Protocol == "local" && !isZFSMount(benchPath) && !isSmoothfsMount(benchPath))

	args := []string{
		"--name=tierd-bench",
		"--filename=" + filename,
		"--size=" + strconv.Itoa(req.SizeMB) + "m",
		"--runtime=" + strconv.Itoa(req.DurationS),
		"--time_based",
		"--rw=" + req.Mode,
		"--bs=" + req.BlockSize,
		"--numjobs=1",
		"--group_reporting",
		"--output-format=json",
	}

	if useDirectIO {
		// libaio is required so that iodepth=32 is honoured (psync ignores it)
		// and so that O_DIRECT buffer alignment is handled correctly for block
		// devices that report a larger logical sector size.
		args = append(args, "--ioengine=libaio", "--direct=1", "--iodepth=32")
	} else {
		args = append(args, "--ioengine=sync", "--iodepth=1")
	}

	if req.Mode == "randrw" {
		args = append(args, "--rwmixread=50")
	}

	// When live data is wanted, request a JSON status block every second.
	// fio explicitly calls fflush() after each status block so they arrive
	// through the stdout pipe in real time (unlike the _bw.log / _iops.log
	// files which use stdio full-buffering and are not flushed until exit).
	if resultFn != nil {
		args = append(args, "--status-interval=1")
	}

	var stderr bytes.Buffer
	cmd := exec.Command("fio", args...)
	cmd.Stderr = &stderr

	// Set up stdout: pipe for live updates, buffer otherwise.
	var stdout bytes.Buffer
	var stdoutPipe io.ReadCloser
	if resultFn != nil {
		var err error
		stdoutPipe, err = cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
		}
	} else {
		cmd.Stdout = &stdout
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fio failed to start: %w", err)
	}

	// Live-data goroutine: decode the stream of JSON status blocks that
	// --status-interval=1 emits once per second, compute instantaneous
	// read/write MB/s and IOPS from consecutive io_kbytes deltas, and call
	// resultFn with the growing slice of DataPoints.  The last decoded block
	// (fio's final summary) is kept for building the Result.
	type pipeResult struct {
		points []DataPoint
		lastFO fioOutput
		err    error
	}
	pipeC := make(chan pipeResult, 1)

	if resultFn != nil {
		bsKB := blockSizeKB(req.BlockSize)
		go func() {
			var res pipeResult
			var prevReadKB, prevWriteKB float64
			elapsed := 0

			// Skip any non-JSON preamble before the first JSON block.
			// fio prints "note: ..." and similar lines to stdout when using
			// --direct=1 on local non-ZFS filesystems; these would cause the
			// first Decode() to fail with "invalid character" errors.
			br := bufio.NewReader(stdoutPipe)
		skipPreamble:
			for {
				b, err := br.ReadByte()
				switch {
				case err != nil:
					// EOF before any JSON — nothing to parse.
					pipeC <- res
					return
				case b == '{':
					br.UnreadByte() //nolint:errcheck
					break skipPreamble
				}
			}

			dec := json.NewDecoder(br)
			for dec.More() {
				var fo fioOutput
				if err := dec.Decode(&fo); err != nil {
					res.err = err
					break
				}
				if len(fo.Jobs) == 0 {
					continue
				}
				elapsed++
				j := fo.Jobs[0]
				readKB := j.Read.IOKbytes
				writeKB := j.Write.IOKbytes
				readMBPS := (readKB - prevReadKB) / 1024.0
				writeMBPS := (writeKB - prevWriteKB) / 1024.0
				readIOPS := (readKB - prevReadKB) / bsKB
				writeIOPS := (writeKB - prevWriteKB) / bsKB
				prevReadKB = readKB
				prevWriteKB = writeKB
				// Always track the latest block for the final result.
				res.lastFO = fo
				// Blocks beyond the configured duration are fio's post-run
				// summary; exclude them from the chart data points.
				if elapsed > req.DurationS {
					continue
				}
				res.points = append(res.points, DataPoint{
					ElapsedS:  elapsed,
					ReadMBPS:  readMBPS,
					WriteMBPS: writeMBPS,
					ReadIOPS:  readIOPS,
					WriteIOPS: writeIOPS,
				})
				pts := make([]DataPoint, len(res.points))
				copy(pts, res.points)
				resultFn(&Result{
					Mode:       req.Mode,
					DurationS:  req.DurationS,
					DataPoints: pts,
				})
			}
			// Drain any unread output so fio isn't blocked on a full pipe
			// buffer — if we broke out of the loop early (decode error),
			// fio may still be writing and cmd.Wait() would deadlock.
			io.Copy(io.Discard, br) //nolint:errcheck
			pipeC <- res
		}()
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var cmdErr error
	if progressFn != nil {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		start := time.Now()
	outer:
		for {
			select {
			case <-ticker.C:
				elapsed := int(time.Since(start).Seconds())
				progressFn(fmt.Sprintf("Running fio... %ds / %ds", elapsed, req.DurationS))
			case cmdErr = <-waitDone:
				break outer
			}
		}
	} else {
		cmdErr = <-waitDone
	}

	if cmdErr != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("fio: %s", strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("fio failed: %w", cmdErr)
	}

	var (
		result    *Result
		resultErr error
	)
	if resultFn != nil {
		live := <-pipeC
		if live.err != nil && len(live.lastFO.Jobs) == 0 {
			return nil, fmt.Errorf("failed to parse fio output: %w", live.err)
		}
		result, resultErr = buildResult(live.lastFO, req)
		if resultErr != nil {
			return nil, resultErr
		}
		result.DataPoints = live.points
	} else {
		result, resultErr = parseOutput(stdout.Bytes(), req)
		if resultErr != nil {
			return nil, resultErr
		}
	}

	result.RemoteTarget = remoteTarget
	return result, nil
}

// blockSizeKB returns the block size in kilobytes for a fio bs string (e.g. "4k" → 4, "1m" → 1024).
func blockSizeKB(bs string) float64 {
	bs = strings.ToLower(bs)
	if strings.HasSuffix(bs, "m") {
		n, _ := strconv.ParseFloat(strings.TrimSuffix(bs, "m"), 64)
		return n * 1024
	}
	if strings.HasSuffix(bs, "k") {
		n, _ := strconv.ParseFloat(strings.TrimSuffix(bs, "k"), 64)
		return n
	}
	n, _ := strconv.ParseFloat(bs, 64)
	return n / 1024
}

// mountFSType returns the filesystem type of the longest-matching mount point
// for path, by scanning /proc/mounts.
func mountFSType(path string) string {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	best, bestFS := "", ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mnt := fields[1]
		if (path == mnt || strings.HasPrefix(path, mnt+"/")) && len(mnt) > len(best) {
			best = mnt
			bestFS = fields[2]
		}
	}
	return bestFS
}

// isZFSMount reports whether path resides on a ZFS filesystem.
func isZFSMount(path string) bool { return mountFSType(path) == "zfs" }

// isSmoothfsMount reports whether path resides on a smoothfs filesystem.
func isSmoothfsMount(path string) bool {
	return mountFSType(path) == "smoothfs"
}

// smoothfsBacking returns the first backing-store mount point for a smoothfs
// tier mount. smoothfs tier mounts follow the pattern /mnt/<pool> with backing
// stores at /mnt/.tierd-backing/<pool>/<tier>/.
func smoothfsBacking(smoothfsPath string) (string, error) {
	pool := filepath.Base(smoothfsPath)
	prefix := filepath.Join("/mnt/.tierd-backing", pool) + "/"
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", fmt.Errorf("cannot read /proc/mounts: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mnt := fields[1]
		if strings.HasPrefix(mnt, prefix) {
			return mnt, nil
		}
	}
	return "", fmt.Errorf("no backing store found under %s", prefix)
}

// mountRemote mounts a remote SMB or NFS share to a temporary directory.
// Returns the mount point path and a cleanup function that unmounts and removes it.
func mountRemote(req Request) (string, func(), error) {
	mountPoint, err := os.MkdirTemp("", "tierd-bench-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create mount point: %w", err)
	}

	cleanup := func() {
		exec.Command("umount", "-l", mountPoint).Run()
		os.RemoveAll(mountPoint)
	}

	var mountArgs []string

	switch req.Protocol {
	case "smb":
		target := fmt.Sprintf("//%s/%s", req.RemoteHost, req.RemoteShare)
		opts := req.MountOptions
		if req.RemoteUser != "" {
			// Write credentials to a temp file so they don't appear in process listings.
			credsFile, err := os.CreateTemp("", "tierd-creds-*")
			if err != nil {
				os.RemoveAll(mountPoint)
				return "", nil, fmt.Errorf("failed to create credentials file: %w", err)
			}
			fmt.Fprintf(credsFile, "username=%s\npassword=%s\n", req.RemoteUser, req.RemotePass)
			credsFile.Close()
			defer os.Remove(credsFile.Name())
			base := "rw,credentials=" + credsFile.Name()
			if opts != "" {
				base += "," + opts
			}
			mountArgs = []string{"-t", "cifs", target, mountPoint, "-o", base}
		} else {
			base := "rw,guest"
			if opts != "" {
				base += "," + opts
			}
			mountArgs = []string{"-t", "cifs", target, mountPoint, "-o", base}
		}

	case "nfs":
		target := fmt.Sprintf("%s:%s", req.RemoteHost, req.RemoteShare)
		opts := req.MountOptions
		if opts == "" {
			opts = nfs.DefaultClientMountOptions
		}
		mountArgs = []string{"-t", "nfs", target, mountPoint, "-o", opts}

	default:
		os.RemoveAll(mountPoint)
		return "", nil, fmt.Errorf("unsupported protocol for remote mount: %s", req.Protocol)
	}

	if out, err := exec.Command("mount", mountArgs...).CombinedOutput(); err != nil {
		os.RemoveAll(mountPoint)
		return "", nil, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}

	return mountPoint, cleanup, nil
}

// fioOutput is a minimal subset of fio's JSON output schema, covering both
// the periodic --status-interval blocks and the final result block.
type fioOutput struct {
	Jobs []struct {
		Read struct {
			IOPS     float64 `json:"iops"`
			BW       float64 `json:"bw"`        // KB/s
			IOKbytes float64 `json:"io_kbytes"` // cumulative KB transferred
			LatNS    struct {
				Mean float64 `json:"mean"`
			} `json:"lat_ns"`
		} `json:"read"`
		Write struct {
			IOPS     float64 `json:"iops"`
			BW       float64 `json:"bw"`
			IOKbytes float64 `json:"io_kbytes"`
			LatNS    struct {
				Mean float64 `json:"mean"`
			} `json:"lat_ns"`
		} `json:"write"`
	} `json:"jobs"`
}

func parseOutput(raw []byte, req Request) (*Result, error) {
	// Skip any non-JSON preamble (e.g. fio warning/note lines printed before
	// the JSON object, which would otherwise break json.Unmarshal).
	if i := bytes.IndexByte(raw, '{'); i > 0 {
		raw = raw[i:]
	}
	var fo fioOutput
	if err := json.Unmarshal(raw, &fo); err != nil {
		return nil, fmt.Errorf("failed to parse fio output: %w", err)
	}
	return buildResult(fo, req)
}

func buildResult(fo fioOutput, req Request) (*Result, error) {
	if len(fo.Jobs) == 0 {
		return nil, fmt.Errorf("fio returned no job results")
	}
	j := fo.Jobs[0]
	return &Result{
		Protocol:   req.Protocol,
		Path:       req.Path,
		ReadIOPS:   j.Read.IOPS,
		WriteIOPS:  j.Write.IOPS,
		ReadBWMB:   j.Read.BW / 1024.0,
		WriteBWMB:  j.Write.BW / 1024.0,
		ReadLatUS:  j.Read.LatNS.Mean / 1000.0,
		WriteLatUS: j.Write.LatNS.Mean / 1000.0,
		DurationS:  req.DurationS,
		Mode:       req.Mode,
		BlockSize:  req.BlockSize,
	}, nil
}
