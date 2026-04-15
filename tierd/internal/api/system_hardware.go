package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Hardware describes the host CPU, memory, and NICs along with current
// utilization. Returned by GET /api/system/hardware.
type Hardware struct {
	CPU  CPUInfo   `json:"cpu"`
	Mem  MemInfo   `json:"mem"`
	NICs []NICInfo `json:"nics"`
}

type CPUInfo struct {
	Model    string  `json:"model"`
	Cores    int     `json:"cores"`
	UsagePct float64 `json:"usage_pct"`
}

type MemInfo struct {
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPct        float64 `json:"used_pct"`
}

type NICInfo struct {
	Name      string `json:"name"`
	Link      string `json:"link"`
	SpeedMbps int    `json:"speed_mbps"`
	MAC       string `json:"mac"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
}

func (h *SystemHandler) getHardware(w http.ResponseWriter, r *http.Request) {
	hw := Hardware{
		CPU:  readCPUInfo(),
		Mem:  readMemInfo(),
		NICs: readNICs(),
	}
	json.NewEncoder(w).Encode(hw)
}

// --- CPU ---

func readCPUInfo() CPUInfo {
	info := CPUInfo{
		Model: cpuModel(),
		Cores: cpuCores(),
	}
	info.UsagePct = sampleCPUUsage(100 * time.Millisecond)
	return info
}

func cpuModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func cpuCores() int {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "processor") {
			count++
		}
	}
	return count
}

// sampleCPUUsage takes two /proc/stat samples `gap` apart and returns
// percent busy across all cores.
func sampleCPUUsage(gap time.Duration) float64 {
	a, ok := readCPUStat()
	if !ok {
		return 0
	}
	time.Sleep(gap)
	b, ok := readCPUStat()
	if !ok {
		return 0
	}
	totalDelta := b.total - a.total
	idleDelta := b.idle - a.idle
	if totalDelta == 0 {
		return 0
	}
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100
}

type cpuStat struct {
	total uint64
	idle  uint64
}

func readCPUStat() (cpuStat, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuStat{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		// Fields after "cpu": user nice system idle iowait irq softirq steal guest guest_nice
		if len(fields) < 5 {
			return cpuStat{}, false
		}
		var total uint64
		var idle uint64
		for i := 1; i < len(fields); i++ {
			n, _ := strconv.ParseUint(fields[i], 10, 64)
			total += n
			if i == 4 || i == 5 { // idle + iowait
				idle += n
			}
		}
		return cpuStat{total: total, idle: idle}, true
	}
	return cpuStat{}, false
}

// --- Memory ---

func readMemInfo() MemInfo {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return MemInfo{}
	}
	vals := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		fields := strings.Fields(parts[1])
		if len(fields) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(fields[0], 10, 64)
		// /proc/meminfo reports kB.
		vals[key] = n * 1024
	}
	total := vals["MemTotal"]
	avail := vals["MemAvailable"]
	if avail == 0 {
		avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	used := total - avail
	var pct float64
	if total > 0 {
		pct = float64(used) / float64(total) * 100
	}
	return MemInfo{
		TotalBytes:     total,
		AvailableBytes: avail,
		UsedBytes:      used,
		UsedPct:        pct,
	}
}

// --- NICs ---

func readNICs() []NICInfo {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil
	}
	nics := make([]NICInfo, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if name == "lo" || skipNIC(name) {
			continue
		}
		nics = append(nics, readNIC(name))
	}
	sort.Slice(nics, func(i, j int) bool { return nics[i].Name < nics[j].Name })
	return nics
}

func skipNIC(name string) bool {
	prefixes := []string{
		"docker", "veth", "br-", "virbr", "tun", "tap",
		"kube", "flannel", "cni", "cali", "weave", "vnet", "ifb",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func readNIC(name string) NICInfo {
	base := filepath.Join("/sys/class/net", name)
	nic := NICInfo{Name: name, Link: "unknown"}

	if v := readSysfsString(filepath.Join(base, "operstate")); v != "" {
		nic.Link = v
	}
	if v := readSysfsString(filepath.Join(base, "address")); v != "" {
		nic.MAC = v
	}
	if v := readSysfsUint(filepath.Join(base, "speed")); v > 0 {
		nic.SpeedMbps = int(v)
	}
	nic.RxBytes = readSysfsUint(filepath.Join(base, "statistics", "rx_bytes"))
	nic.TxBytes = readSysfsUint(filepath.Join(base, "statistics", "tx_bytes"))
	return nic
}

func readSysfsString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readSysfsUint(path string) uint64 {
	s := readSysfsString(path)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
