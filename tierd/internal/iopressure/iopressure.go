package iopressure

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const DefaultPath = "/proc/pressure/io"

// Sample is a parsed Linux PSI IO sample.
type Sample struct {
	SomeAvg10  float64
	SomeAvg60  float64
	SomeAvg300 float64
	FullAvg10  float64
	FullAvg60  float64
	FullAvg300 float64
}

// ReadDefault reads the host's current IO pressure sample.
func ReadDefault() (Sample, error) {
	return Read(DefaultPath)
}

// Read parses a Linux /proc/pressure/io file.
func Read(path string) (Sample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Sample{}, err
	}
	return Parse(string(data))
}

// Parse parses Linux PSI IO pressure text.
func Parse(text string) (Sample, error) {
	var sample Sample
	var sawSome, sawFull bool
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var target *float64
		switch fields[0] {
		case "some":
			sawSome = true
		case "full":
			sawFull = true
		default:
			continue
		}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch fields[0] + "." + key {
			case "some.avg10":
				target = &sample.SomeAvg10
			case "some.avg60":
				target = &sample.SomeAvg60
			case "some.avg300":
				target = &sample.SomeAvg300
			case "full.avg10":
				target = &sample.FullAvg10
			case "full.avg60":
				target = &sample.FullAvg60
			case "full.avg300":
				target = &sample.FullAvg300
			default:
				target = nil
			}
			if target == nil {
				continue
			}
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return Sample{}, fmt.Errorf("parse %s: %w", field, err)
			}
			*target = parsed
		}
	}
	if !sawSome || !sawFull {
		return Sample{}, fmt.Errorf("missing PSI some/full lines")
	}
	return sample, nil
}

// High reports whether current IO pressure is too high for cold maintenance.
func (s Sample) High(avg10Threshold float64) bool {
	return s.SomeAvg10 >= avg10Threshold || s.FullAvg10 >= avg10Threshold
}

// Short returns a compact string suitable for logs.
func (s Sample) Short() string {
	return fmt.Sprintf("some.avg10=%.2f full.avg10=%.2f", s.SomeAvg10, s.FullAvg10)
}
