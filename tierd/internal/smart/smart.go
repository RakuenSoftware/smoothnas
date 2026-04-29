// Package smart provides SMART data collection and parsing via smartctl.
package smart

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Data represents the full SMART data for a disk.
type Data struct {
	DevicePath    string      `json:"device_path"`
	ModelName     string      `json:"model_name"`
	SerialNumber  string      `json:"serial_number"`
	FirmwareVer   string      `json:"firmware_version"`
	HealthPassed  bool        `json:"health_passed"`
	Temperature   int         `json:"temperature_celsius"`
	PowerOnHours  int         `json:"power_on_hours"`
	Attributes    []Attribute `json:"attributes"`
	ErrorLogCount int         `json:"error_log_count"`
}

// Attribute represents a single SMART attribute.
type Attribute struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Current   int    `json:"current"`
	Worst     int    `json:"worst"`
	Threshold int    `json:"threshold"`
	RawValue  int64  `json:"raw_value"`
	RawString string `json:"raw_string"`
	Flags     string `json:"flags"`
}

// TestResult represents a SMART self-test result.
type TestResult struct {
	Type             string `json:"type"`
	Status           string `json:"status"`
	RemainingPercent int    `json:"remaining_percent"`
	LifetimeHours    int    `json:"lifetime_hours"`
	LBAOfFirstError  int64  `json:"lba_of_first_error"`
}

// smartctlJSON is the top-level structure of smartctl --json output.
type smartctlJSON struct {
	ModelName       string `json:"model_name"`
	SerialNumber    string `json:"serial_number"`
	FirmwareVersion string `json:"firmware_version"`
	SmartStatus     struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	ATASmartAttributes struct {
		Table []struct {
			ID     int    `json:"id"`
			Name   string `json:"name"`
			Value  int    `json:"value"`
			Worst  int    `json:"worst"`
			Thresh int    `json:"thresh"`
			Raw    struct {
				Value  int64  `json:"value"`
				String string `json:"string"`
			} `json:"raw"`
			Flags struct {
				String string `json:"string"`
			} `json:"flags"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	ATASmartErrorLog struct {
		Summary struct {
			Count int `json:"count"`
		} `json:"summary"`
	} `json:"ata_smart_error_log"`
	ATASmartSelfTestLog struct {
		Standard struct {
			Table []struct {
				Type struct {
					String string `json:"string"`
				} `json:"type"`
				Status struct {
					String           string `json:"string"`
					Passed           bool   `json:"passed"`
					RemainingPercent int    `json:"remaining_percent"`
				} `json:"status"`
				LifetimeHours int   `json:"lifetime_hours"`
				LBA           int64 `json:"lba"`
			} `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	NvmeSmartHealthInformationLog struct {
		Temperature     int `json:"temperature"`
		PowerOnHours    int `json:"power_on_hours"`
		MediaErrors     int `json:"media_errors"`
		CriticalWarning int `json:"critical_warning"`
		AvailableSpare  int `json:"available_spare"`
		PercentageUsed  int `json:"percentage_used"`
	} `json:"nvme_smart_health_information_log"`
}

// ReadData collects SMART data from a disk via smartctl --json.
func ReadData(devicePath string) (*Data, error) {
	if !isValidPath(devicePath) {
		return nil, fmt.Errorf("invalid device path: %s", devicePath)
	}

	// smartctl returns non-zero exit codes for various warnings; we still parse the output.
	out, _ := exec.Command("smartctl", smartctlAllJSONArgs(devicePath)...).Output()
	if len(out) == 0 {
		return nil, fmt.Errorf("smartctl returned no output for %s", devicePath)
	}

	var raw smartctlJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse smartctl output: %w", err)
	}

	data := &Data{
		DevicePath:   devicePath,
		ModelName:    raw.ModelName,
		SerialNumber: raw.SerialNumber,
		FirmwareVer:  raw.FirmwareVersion,
		HealthPassed: raw.SmartStatus.Passed,
	}

	// ATA drives.
	if raw.Temperature.Current > 0 {
		data.Temperature = raw.Temperature.Current
	}
	if raw.PowerOnTime.Hours > 0 {
		data.PowerOnHours = raw.PowerOnTime.Hours
	}

	for _, attr := range raw.ATASmartAttributes.Table {
		rawValue := attr.Raw.Value
		// Temperature_Celsius (ID 194) raw value is a packed 48-bit field
		// where the actual temperature is in the lowest 8 bits and higher
		// bytes contain min/max/trip-point data. Mask to extract the real temp.
		if attr.ID == 194 {
			rawValue = rawValue & 0xFF
		}
		data.Attributes = append(data.Attributes, Attribute{
			ID:        attr.ID,
			Name:      attr.Name,
			Current:   attr.Value,
			Worst:     attr.Worst,
			Threshold: attr.Thresh,
			RawValue:  rawValue,
			RawString: attr.Raw.String,
			Flags:     attr.Flags.String,
		})
	}

	data.ErrorLogCount = raw.ATASmartErrorLog.Summary.Count

	// NVMe drives: use NVMe-specific fields.
	if raw.NvmeSmartHealthInformationLog.Temperature > 0 {
		data.Temperature = raw.NvmeSmartHealthInformationLog.Temperature
	}
	if raw.NvmeSmartHealthInformationLog.PowerOnHours > 0 {
		data.PowerOnHours = raw.NvmeSmartHealthInformationLog.PowerOnHours
	}
	if raw.NvmeSmartHealthInformationLog.MediaErrors > 0 {
		data.ErrorLogCount = raw.NvmeSmartHealthInformationLog.MediaErrors
	}

	// Add synthetic NVMe attributes for the alarm system to evaluate.
	if raw.NvmeSmartHealthInformationLog.AvailableSpare > 0 {
		data.Attributes = append(data.Attributes, Attribute{
			ID:       231,
			Name:     "Available_Spare",
			Current:  raw.NvmeSmartHealthInformationLog.AvailableSpare,
			RawValue: int64(raw.NvmeSmartHealthInformationLog.AvailableSpare),
		})
	}
	if raw.NvmeSmartHealthInformationLog.PercentageUsed > 0 {
		data.Attributes = append(data.Attributes, Attribute{
			ID:       232,
			Name:     "Percentage_Used",
			Current:  raw.NvmeSmartHealthInformationLog.PercentageUsed,
			RawValue: int64(raw.NvmeSmartHealthInformationLog.PercentageUsed),
		})
	}

	return data, nil
}

// GetTestResults returns SMART self-test history for a disk.
func GetTestResults(devicePath string) ([]TestResult, error) {
	if !isValidPath(devicePath) {
		return nil, fmt.Errorf("invalid device path: %s", devicePath)
	}

	out, _ := exec.Command("smartctl", smartctlAllJSONArgs(devicePath)...).Output()
	if len(out) == 0 {
		return nil, fmt.Errorf("smartctl returned no output for %s", devicePath)
	}

	var raw smartctlJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse smartctl output: %w", err)
	}

	var results []TestResult
	for _, t := range raw.ATASmartSelfTestLog.Standard.Table {
		status := "passed"
		if !t.Status.Passed {
			status = "failed"
		}
		results = append(results, TestResult{
			Type:             t.Type.String,
			Status:           status,
			RemainingPercent: t.Status.RemainingPercent,
			LifetimeHours:    t.LifetimeHours,
			LBAOfFirstError:  t.LBA,
		})
	}

	return results, nil
}

// StartTest initiates a SMART self-test. testType is "short" or "long".
func StartTest(devicePath, testType string) error {
	if !isValidPath(devicePath) {
		return fmt.Errorf("invalid device path: %s", devicePath)
	}
	if testType != "short" && testType != "long" {
		return fmt.Errorf("invalid test type: %s (must be 'short' or 'long')", testType)
	}

	cmd := exec.Command("smartctl", "--test="+testType, devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("smartctl test: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func smartctlAllJSONArgs(devicePath string) []string {
	return []string{"-n", "standby", "--all", "--json", devicePath}
}

func isValidPath(path string) bool {
	if strings.HasPrefix(path, "/dev/sd") {
		return true
	}
	if strings.HasPrefix(path, "/dev/nvme") {
		return true
	}
	return false
}
