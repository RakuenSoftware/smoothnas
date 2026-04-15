package zfsmgd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// IOStatProvider abstracts the host I/O utilization sample so the movement
// worker can throttle based on device busyness.
type IOStatProvider interface {
	AverageUtilPct(ctx context.Context, devices []string) (float64, error)
}

// ExecIOStat is the production IOStatProvider. It runs `iostat` from the
// sysstat package and parses its JSON output.
type ExecIOStat struct{}

func (ExecIOStat) AverageUtilPct(ctx context.Context, devices []string) (float64, error) {
	if len(devices) == 0 {
		return 0, nil
	}
	cmd := exec.CommandContext(ctx, "iostat", "-x", "-o", "JSON", "1", "1")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("iostat: %w", err)
	}
	return parseIOStatJSON(out, devices)
}

type iostatReport struct {
	Sysstat struct {
		Hosts []struct {
			Statistics []struct {
				Disk []iostatDisk `json:"disk"`
			} `json:"statistics"`
		} `json:"hosts"`
	} `json:"sysstat"`
}

type iostatDisk struct {
	Name string  `json:"disk_device"`
	Util float64 `json:"util"`
}

func parseIOStatJSON(raw []byte, devices []string) (float64, error) {
	var report iostatReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return 0, fmt.Errorf("parse iostat json: %w", err)
	}
	wanted := make(map[string]struct{}, len(devices))
	for _, d := range devices {
		wanted[normalizeDevName(d)] = struct{}{}
	}
	var sum float64
	var count int
	for _, host := range report.Sysstat.Hosts {
		for _, stat := range host.Statistics {
			for _, disk := range stat.Disk {
				if _, ok := wanted[normalizeDevName(disk.Name)]; !ok {
					continue
				}
				sum += disk.Util
				count++
			}
		}
	}
	if count == 0 {
		return 0, nil
	}
	return sum / float64(count), nil
}

func normalizeDevName(name string) string {
	name = strings.TrimSpace(name)
	return strings.TrimPrefix(name, "/dev/")
}
