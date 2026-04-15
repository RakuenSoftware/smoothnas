// Package benchmark — system (CPU + memory) benchmarks via sysbench.
//
// NAS relevance:
//   - CPU single-core: compression (ZFS/Btrfs), encryption (SMB3), checksums
//   - CPU multi-core: concurrent client throughput, parity calculation, scrub
//   - Memory bandwidth: ARC/page-cache throughput, large file copy buffers
package benchmark

import (
	"bufio"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// SystemRequest holds parameters for a CPU/memory benchmark.
type SystemRequest struct {
	DurationS int `json:"duration"` // seconds per sub-test, 5–120
}

// SystemResult holds parsed sysbench output for all sub-tests.
type SystemResult struct {
	CPUSingleCore *CPUResult    `json:"cpu_single_core"`
	CPUMultiCore  *CPUResult    `json:"cpu_multi_core"`
	Memory        *MemoryResult `json:"memory"`
}

// CPUResult holds parsed sysbench CPU output.
type CPUResult struct {
	Threads       int     `json:"threads"`
	EventsPerSec  float64 `json:"events_per_sec"`
	TotalEvents   int     `json:"total_events"`
	TotalTimeSec  float64 `json:"total_time_sec"`
	LatencyAvgMS  float64 `json:"latency_avg_ms"`
	LatencyP95MS  float64 `json:"latency_p95_ms"`
	LatencyMaxMS  float64 `json:"latency_max_ms"`
}

// MemoryResult holds parsed sysbench memory output.
type MemoryResult struct {
	ThroughputMBs float64 `json:"throughput_mbs"`
	TotalOps      int     `json:"total_ops"`
	OpsPerSec     float64 `json:"ops_per_sec"`
	TotalTimeSec  float64 `json:"total_time_sec"`
	BlockSize     string  `json:"block_size"`
	TotalSize     string  `json:"total_size"`
}

// ValidateSystem checks system benchmark request fields.
func (r *SystemRequest) ValidateSystem() error {
	if r.DurationS < 5 || r.DurationS > 120 {
		return fmt.Errorf("duration must be between 5 and 120 seconds")
	}
	return nil
}

// EnsureSysbench installs sysbench via apt if not already present.
func EnsureSysbench() error {
	if _, err := exec.LookPath("sysbench"); err == nil {
		return nil
	}
	cmd := exec.Command("apt-get", "install", "-y", "sysbench")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install sysbench: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// RunSystem executes CPU and memory benchmarks sequentially.
// progressFn is called with status updates; it may be nil.
func RunSystem(req SystemRequest, progressFn func(string)) (*SystemResult, error) {
	if err := EnsureSysbench(); err != nil {
		return nil, err
	}

	result := &SystemResult{}

	// CPU single-core
	if progressFn != nil {
		progressFn("Running CPU single-core benchmark...")
	}
	cpu1, err := runSysbenchCPU(1, req.DurationS)
	if err != nil {
		return nil, fmt.Errorf("cpu single-core: %w", err)
	}
	result.CPUSingleCore = cpu1

	// CPU multi-core
	threads := runtime.NumCPU()
	if progressFn != nil {
		progressFn(fmt.Sprintf("Running CPU multi-core benchmark (%d threads)...", threads))
	}
	cpuN, err := runSysbenchCPU(threads, req.DurationS)
	if err != nil {
		return nil, fmt.Errorf("cpu multi-core: %w", err)
	}
	result.CPUMultiCore = cpuN

	// Memory bandwidth
	if progressFn != nil {
		progressFn("Running memory bandwidth benchmark...")
	}
	mem, err := runSysbenchMemory(req.DurationS)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	result.Memory = mem

	return result, nil
}

func runSysbenchCPU(threads, durationS int) (*CPUResult, error) {
	args := []string{
		"cpu",
		"--threads=" + strconv.Itoa(threads),
		"--time=" + strconv.Itoa(durationS),
		"--cpu-max-prime=20000",
		"run",
	}
	out, err := exec.Command("sysbench", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sysbench: %s", strings.TrimSpace(string(out)))
	}
	return parseSysbenchCPU(string(out), threads)
}

func runSysbenchMemory(durationS int) (*MemoryResult, error) {
	args := []string{
		"memory",
		"--threads=1",
		"--time=" + strconv.Itoa(durationS),
		"--memory-block-size=1K",
		"--memory-total-size=100T",
		"--memory-oper=write",
		"--memory-access-mode=seq",
		"run",
	}
	out, err := exec.Command("sysbench", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sysbench: %s", strings.TrimSpace(string(out)))
	}
	return parseSysbenchMemory(string(out))
}

func parseSysbenchCPU(output string, threads int) (*CPUResult, error) {
	r := &CPUResult{Threads: threads}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if v, ok := extractFloat(line, "events per second:"); ok {
			r.EventsPerSec = v
		} else if v, ok := extractInt(line, "total number of events:"); ok {
			r.TotalEvents = v
		} else if v, ok := extractFloat(line, "total time:"); ok {
			r.TotalTimeSec = v
		} else if strings.Contains(line, "avg:") && r.LatencyAvgMS == 0 {
			r.LatencyAvgMS = extractLatencyField(line, "avg:")
		} else if strings.Contains(line, "95th percentile:") {
			if v, ok := extractFloat(line, "95th percentile:"); ok {
				r.LatencyP95MS = v
			}
		} else if strings.Contains(line, "max:") && r.LatencyMaxMS == 0 && strings.Contains(output[strings.Index(output, line)-40:strings.Index(output, line)+len(line)], "Latency") {
			// Only capture max from the Latency section
		}
	}
	// Re-parse for latency block
	inLatency := false
	scanner = bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Latency (ms):") || strings.HasPrefix(line, "Latency (ms)") {
			inLatency = true
			continue
		}
		if inLatency {
			if v, ok := extractFloat(line, "avg:"); ok {
				r.LatencyAvgMS = v
			}
			if v, ok := extractFloat(line, "max:"); ok {
				r.LatencyMaxMS = v
			}
			if strings.HasPrefix(line, "95th percentile:") {
				if v, ok := extractFloat(line, "95th percentile:"); ok {
					r.LatencyP95MS = v
				}
			}
			if line == "" || strings.HasPrefix(line, "Threads fairness") {
				inLatency = false
			}
		}
	}
	return r, nil
}

func parseSysbenchMemory(output string) (*MemoryResult, error) {
	r := &MemoryResult{
		BlockSize: "1K",
		TotalSize: "100T",
	}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// "102400.00 MiB transferred (12345.67 MiB/sec)"
		if strings.Contains(line, "MiB transferred") {
			if idx := strings.Index(line, "("); idx >= 0 {
				rest := line[idx+1:]
				if end := strings.Index(rest, " MiB/sec)"); end >= 0 {
					if v, err := strconv.ParseFloat(rest[:end], 64); err == nil {
						r.ThroughputMBs = v
					}
				}
			}
		}
		if v, ok := extractFloat(line, "total number of events:"); ok {
			r.TotalOps = int(v)
		}
		if v, ok := extractFloat(line, "total time:"); ok {
			r.TotalTimeSec = v
		}
	}
	if r.TotalOps > 0 && r.TotalTimeSec > 0 {
		r.OpsPerSec = float64(r.TotalOps) / r.TotalTimeSec
	}
	return r, nil
}

func extractFloat(line, prefix string) (float64, bool) {
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(line[idx+len(prefix):])
	// Remove trailing units like "s", "ms", etc.
	rest = strings.TrimRight(rest, "smSM ")
	v, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func extractInt(line, prefix string) (int, bool) {
	v, ok := extractFloat(line, prefix)
	if !ok {
		return 0, false
	}
	return int(v), true
}

func extractLatencyField(line, field string) float64 {
	idx := strings.Index(line, field)
	if idx < 0 {
		return 0
	}
	rest := strings.TrimSpace(line[idx+len(field):])
	v, _ := strconv.ParseFloat(rest, 64)
	return v
}
