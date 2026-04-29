package zfs

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// HealthAlert is a ZFS health problem found by the background monitor.
type HealthAlert struct {
	PoolName string // empty for ARC/global alerts
	Severity string // "warning" or "critical"
	Message  string
}

// ARCStats holds selected counters from /proc/spl/kstat/zfs/arcstats.
type ARCStats struct {
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	L2Hits   uint64 `json:"l2_hits"`
	L2Misses uint64 `json:"l2_misses"`
	C        uint64 `json:"c"`     // current ARC target size (bytes)
	CMin     uint64 `json:"c_min"` // ARC minimum size (bytes)
	CMax     uint64 `json:"c_max"` // ARC maximum size (bytes)
}

// PoolSummary is a lightweight pool overview for /api/system/status.
type PoolSummary struct {
	Name     string `json:"name"`
	Health   string `json:"health"`
	HasSLOG  bool   `json:"has_slog"`
	HasL2ARC bool   `json:"has_l2arc"`
}

// ReadARCStats reads ARC statistics from /proc/spl/kstat/zfs/arcstats.
// Returns nil if the file is not available (ZFS not loaded).
func ReadARCStats() (*ARCStats, error) {
	f, err := os.Open("/proc/spl/kstat/zfs/arcstats")
	if err != nil {
		return nil, nil // ZFS not loaded; non-fatal
	}
	defer f.Close()

	stats := &ARCStats{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		name := fields[0]
		val, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "hits":
			stats.Hits = val
		case "misses":
			stats.Misses = val
		case "l2_hits":
			stats.L2Hits = val
		case "l2_misses":
			stats.L2Misses = val
		case "c":
			stats.C = val
		case "c_min":
			stats.CMin = val
		case "c_max":
			stats.CMax = val
		}
	}
	return stats, scanner.Err()
}

// CheckPoolAlerts returns health alerts for all ZFS pools by inspecting
// pool health, resilver state, and checksum errors.
func CheckPoolAlerts() []HealthAlert {
	pools, err := ListPools()
	if err != nil || len(pools) == 0 {
		return nil
	}

	var alerts []HealthAlert
	for _, p := range pools {
		detail, err := DetailPool(p.Name)
		if err != nil {
			continue
		}

		switch detail.Health {
		case "DEGRADED":
			alerts = append(alerts, HealthAlert{
				PoolName: detail.Name,
				Severity: "warning",
				Message:  "pool " + detail.Name + " is DEGRADED",
			})
		case "FAULTED", "OFFLINE", "REMOVED":
			alerts = append(alerts, HealthAlert{
				PoolName: detail.Name,
				Severity: "critical",
				Message:  "pool " + detail.Name + " is " + detail.Health,
			})
		}

		if strings.Contains(detail.ScanStatus, "resilver in progress") {
			alerts = append(alerts, HealthAlert{
				PoolName: detail.Name,
				Severity: "warning",
				Message:  "pool " + detail.Name + " is resilvering",
			})
		}

		if HasChecksumErrorsInLayout(detail.VdevLayout) {
			alerts = append(alerts, HealthAlert{
				PoolName: detail.Name,
				Severity: "warning",
				Message:  "pool " + detail.Name + " has non-zero checksum errors",
			})
		}
	}
	return alerts
}

// CheckARCAlerts returns ARC health alerts given a populated ARCStats.
// It is a pure function so it can be unit-tested without a live ZFS system.
func CheckARCAlerts(stats *ARCStats) []HealthAlert {
	if stats == nil {
		return nil
	}

	var alerts []HealthAlert

	// L2ARC hit-rate alert: only fire when the L2ARC has been exercised enough
	// to make a meaningful measurement (at least 1000 accesses) and the hit
	// rate is below 10 %.
	l2Total := stats.L2Hits + stats.L2Misses
	if l2Total >= 1000 {
		hitRate := float64(stats.L2Hits) / float64(l2Total)
		if hitRate < 0.10 {
			alerts = append(alerts, HealthAlert{
				Severity: "warning",
				Message:  "L2ARC hit rate is below 10% — cache may be ineffective",
			})
		}
	}

	// ARC pressure alert: fire when the ARC has been pushed to (or below) its
	// minimum size AND the ARC has been exercised enough to make the signal
	// meaningful. Without the activity guard, a system with no ZFS pools (and
	// therefore ~0 ARC usage) permanently trips this alert: ZFS initialises
	// c == c_min by default, so c <= c_min is trivially true from boot.
	arcTotal := stats.Hits + stats.Misses
	if stats.CMin > 0 && stats.C <= stats.CMin && arcTotal >= 1000 {
		alerts = append(alerts, HealthAlert{
			Severity: "warning",
			Message:  "ARC has been pushed to its minimum size — system is under memory pressure",
		})
	}

	return alerts
}

// GetPoolsSummary returns a lightweight summary of all ZFS pools for use in
// /api/system/status. Returns nil if no pools exist or ZFS is not available.
func GetPoolsSummary() []PoolSummary {
	pools, err := ListPools()
	if err != nil || len(pools) == 0 {
		return nil
	}

	summaries := make([]PoolSummary, 0, len(pools))
	for _, p := range pools {
		detail, err := DetailPool(p.Name)
		if err != nil {
			summaries = append(summaries, PoolSummary{
				Name:   p.Name,
				Health: p.Health,
			})
			continue
		}
		summaries = append(summaries, PoolSummary{
			Name:     detail.Name,
			Health:   detail.Health,
			HasSLOG:  HasSLOGInLayout(detail.VdevLayout),
			HasL2ARC: HasL2ARCInLayout(detail.VdevLayout),
		})
	}
	return summaries
}

// HasSLOGInLayout reports whether the vdev layout string contains a log
// (SLOG) device section. The "logs" keyword appears on its own line in
// `zpool status` output when a log device is present.
func HasSLOGInLayout(layout string) bool {
	for _, line := range strings.Split(layout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "logs" {
			return true
		}
	}
	return false
}

// HasL2ARCInLayout reports whether the vdev layout string contains a cache
// (L2ARC) device section.
func HasL2ARCInLayout(layout string) bool {
	for _, line := range strings.Split(layout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "cache" {
			return true
		}
	}
	return false
}

// HasSpecialVdevInLayout reports whether the vdev layout string contains a
// special allocation class vdev. Raw ZFS pools need this for the spindown
// metadata-on-SSD invariant.
func HasSpecialVdevInLayout(layout string) bool {
	for _, line := range strings.Split(layout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "special" {
			return true
		}
	}
	return false
}

// HasChecksumErrorsInLayout reports whether any device in the vdev layout
// has a non-zero checksum error count. The layout lines look like:
//
//	NAME        STATE     READ WRITE CKSUM
//	tank        ONLINE       0     0     0
//	  raidz1-0  ONLINE       0     0     0
//	    sda     ONLINE       0     0     0
//
// We skip the header line and look for a non-zero 5th field.
func HasChecksumErrorsInLayout(layout string) bool {
	for _, line := range strings.Split(layout, "\n") {
		fields := strings.Fields(line)
		// Expect at least: NAME STATE READ WRITE CKSUM
		if len(fields) < 5 {
			continue
		}
		// Skip the header row.
		if fields[0] == "NAME" {
			continue
		}
		// The CKSUM column is fields[4].
		if fields[4] != "0" && fields[4] != "-" {
			return true
		}
	}
	return false
}
