// Package tuning applies kernel and network performance parameters at startup.
// Each parameter is written only when it moves the kernel value toward the
// target, so the functions are safe to call on every boot and never override
// values the operator has already set beyond our defaults.
package tuning

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type kernelParam struct {
	path   string
	target string
	// multiField: value is a space-separated triple; compare on the last field
	// only (e.g. tcp_rmem / tcp_wmem).
	multiField bool
	// lowerIsBetter: write only when current > target (e.g. vfs_cache_pressure).
	lowerIsBetter bool
}

var networkParams = []kernelParam{
	// TCP socket buffer ceilings: let the kernel auto-tune up to 128 MB on
	// fast NICs. The defaults (~212 KB) are tuned for commodity 1 GbE.
	{"/proc/sys/net/core/rmem_max", "134217728", false, false},
	{"/proc/sys/net/core/wmem_max", "134217728", false, false},

	// tcp_rmem / tcp_wmem: min / default / max. Compare on the max field.
	{"/proc/sys/net/ipv4/tcp_rmem", "4096 87380 134217728", true, false},
	{"/proc/sys/net/ipv4/tcp_wmem", "4096 65536 134217728", true, false},

	// NIC RX back-pressure queue. Default of 1000 drops frames under bursty
	// multi-client load.
	{"/proc/sys/net/core/netdev_max_backlog", "5000", false, false},

	// sunrpc TCP slot table: how many in-flight RPCs a single NFS TCP
	// connection may carry. The upstream default of 2 collapses small-file
	// NFS throughput to ~0.8 Gbps on a 2.5 Gbps link because per-file
	// LOOKUP/OPEN/READ/CLOSE RPCs serialise. Measured on the backup path
	// (single-connection NFSv4.2 pull from 192.168.1.254): raising this to
	// 128 moved sustained rate from 2.05 Gbps to 2.30 Gbps (82 % → 92 % of
	// line) on the same 14 GB subtree, with no destination side back-pressure.
	// Affects new NFS mounts only; set at boot before the backup path mounts.
	{"/sys/module/sunrpc/parameters/tcp_slot_table_entries", "128", false, false},

	// Dirty page flush thresholds. Background 5 % starts writeback early;
	// hard cap 20 % prevents write stalls from a single large flush.
	{"/proc/sys/vm/dirty_background_ratio", "5", false, false},
	{"/proc/sys/vm/dirty_ratio", "20", false, false},

	// VFS metadata cache pressure. Default 100 aggressively reclaims
	// dentry/inode cache. 50 keeps directory listings and stat results in
	// RAM much longer — on a NAS the same paths are stat'd repeatedly by
	// every connected client.
	{"/proc/sys/vm/vfs_cache_pressure", "50", false, true},
}

// defaultReadAheadKB is the read-ahead we apply to md arrays and their member
// drives. 4 MB matches the backup copy buffer size and gives the block layer
// enough runway to keep sequential reads from stalling on rotational media.
const defaultReadAheadKB = 4096

// ApplyNetworkTuning raises kernel networking and VM parameters for NAS
// throughput and sets vm.min_free_kbytes scaled to total RAM. Each write is
// skipped when the current value is already at or beyond the target.
// Errors are logged but never fatal — a missing /proc entry (e.g. inside a
// container) is non-fatal.
func ApplyNetworkTuning() {
	for _, p := range networkParams {
		if err := applyParam(p); err != nil {
			log.Printf("tuning: %s: %v", p.path, err)
		}
	}
	if err := applyMinFreeKbytes(); err != nil {
		log.Printf("tuning: vm.min_free_kbytes: %v", err)
	}
}

// ApplyBlockTuning raises read_ahead_kb for every md array and its member
// drives to defaultReadAheadKB. The kernel default of 128 KB is tuned for
// latency-sensitive workloads; sequential NAS transfers benefit from a much
// larger prefetch window that fills the page cache ahead of client reads.
// Only raises — never lowers a value already above the target.
func ApplyBlockTuning() {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		log.Printf("tuning: read /sys/block: %v", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !isTunableBlock(name) {
			continue
		}
		path := "/sys/block/" + name + "/queue/read_ahead_kb"
		p := kernelParam{path: path, target: strconv.Itoa(defaultReadAheadKB)}
		if err := applyParam(p); err != nil {
			log.Printf("tuning: %s: %v", path, err)
		}
	}
}

// isTunableBlock reports whether the block device name is one we should tune:
// md arrays and common disk types (sd*, nvme*, vd*, xvd*). Excludes loop,
// ram, sr, and dm devices.
func isTunableBlock(name string) bool {
	for _, prefix := range []string{"md", "sd", "nvme", "vd", "xvd"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// applyMinFreeKbytes sets vm.min_free_kbytes to 3 % of total RAM, clamped
// to [65536, 1048576] KiB (64 MB – 1 GB). This reserves a small buffer so
// the kernel never stalls page allocation under heavy NAS load.
func applyMinFreeKbytes() error {
	memKB, err := readMemTotalKB()
	if err != nil {
		return fmt.Errorf("read MemTotal: %w", err)
	}

	target := memKB * 3 / 100
	const minKB, maxKB int64 = 65536, 1048576
	if target < minKB {
		target = minKB
	}
	if target > maxKB {
		target = maxKB
	}

	return applyParam(kernelParam{
		path:   "/proc/sys/vm/min_free_kbytes",
		target: strconv.FormatInt(target, 10),
	})
}

func readMemTotalKB() (int64, error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		return strconv.ParseInt(fields[1], 10, 64)
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

func applyParam(p kernelParam) error {
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	current := strings.TrimSpace(string(raw))

	curCmp, tgtCmp := current, p.target
	if p.multiField {
		curCmp = lastField(current)
		tgtCmp = lastField(p.target)
	}

	curVal, err := strconv.ParseInt(curCmp, 10, 64)
	if err != nil {
		return fmt.Errorf("parse current value %q: %w", curCmp, err)
	}
	tgtVal, err := strconv.ParseInt(tgtCmp, 10, 64)
	if err != nil {
		return fmt.Errorf("parse target value %q: %w", tgtCmp, err)
	}

	if p.lowerIsBetter {
		if curVal <= tgtVal {
			return nil // already at or below target
		}
	} else {
		if curVal >= tgtVal {
			return nil // already at or above target
		}
	}

	if err := os.WriteFile(p.path, []byte(p.target+"\n"), 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	log.Printf("tuning: %s: %s -> %s", filepath.Base(p.path), curCmp, tgtCmp)
	return nil
}

func lastField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return s
	}
	return fields[len(fields)-1]
}
