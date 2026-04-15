package tiering_test

import (
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
)

// TestActivityBandConstants verifies the canonical band values are the strings
// documented in the proposal and that IsValidActivityBand classifies them
// correctly.
func TestActivityBandConstants(t *testing.T) {
	valid := []string{
		tiering.ActivityBandHot,
		tiering.ActivityBandWarm,
		tiering.ActivityBandCold,
		tiering.ActivityBandIdle,
	}
	expected := []string{"hot", "warm", "cold", "idle"}
	for i, v := range valid {
		if v != expected[i] {
			t.Errorf("band[%d] = %q, want %q", i, v, expected[i])
		}
		if !tiering.IsValidActivityBand(v) {
			t.Errorf("IsValidActivityBand(%q) = false, want true", v)
		}
	}

	// Unknown values must be rejected.
	invalid := []string{"scorching", "frigid", "", "HOT", "Hot"}
	for _, v := range invalid {
		if tiering.IsValidActivityBand(v) {
			t.Errorf("IsValidActivityBand(%q) = true, want false", v)
		}
	}
}

func TestActivityTrendConstants(t *testing.T) {
	valid := []string{
		tiering.ActivityTrendRising,
		tiering.ActivityTrendStable,
		tiering.ActivityTrendFalling,
	}
	expected := []string{"rising", "stable", "falling"}
	for i, v := range valid {
		if v != expected[i] {
			t.Errorf("trend[%d] = %q, want %q", i, v, expected[i])
		}
		if !tiering.IsValidActivityTrend(v) {
			t.Errorf("IsValidActivityTrend(%q) = false, want true", v)
		}
	}

	// Unknown values must be rejected.
	invalid := []string{"increasing", "", "RISING"}
	for _, v := range invalid {
		if tiering.IsValidActivityTrend(v) {
			t.Errorf("IsValidActivityTrend(%q) = true, want false", v)
		}
	}
}

// TestMdadmActivityBandDerivationRules documents the mdadm/LVM band derivation
// rules from the proposal and verifies that an example bucketing implementation
// produces the correct bands for sample inputs.
//
// The proposal defines:
//   - Hot  = top quartile (IOPS > 75th percentile of the tier instance)
//   - Warm = second quartile (50th–75th percentile)
//   - Cold = third quartile (25th–50th percentile)
//   - Idle = bottom quartile or zero-rate regions
func TestMdadmActivityBandDerivationRules(t *testing.T) {
	// classifyMdadmBand is a local implementation of the mdadm derivation rule.
	// It takes the region's IOPS and the top-percentile IOPS for the tier
	// instance and returns the appropriate band.
	classifyMdadmBand := func(regionIOPS, topPercentileIOPS float64) string {
		if topPercentileIOPS == 0 || regionIOPS == 0 {
			return tiering.ActivityBandIdle
		}
		ratio := regionIOPS / topPercentileIOPS
		switch {
		case ratio > 0.75:
			return tiering.ActivityBandHot
		case ratio > 0.50:
			return tiering.ActivityBandWarm
		case ratio > 0.25:
			return tiering.ActivityBandCold
		default:
			return tiering.ActivityBandIdle
		}
	}

	tests := []struct {
		regionIOPS float64
		topIOPS    float64
		wantBand   string
	}{
		{100, 100, tiering.ActivityBandHot},  // top quartile
		{80, 100, tiering.ActivityBandHot},   // top quartile
		{76, 100, tiering.ActivityBandHot},   // just above 75th
		{60, 100, tiering.ActivityBandWarm},  // second quartile
		{51, 100, tiering.ActivityBandWarm},  // second quartile
		{40, 100, tiering.ActivityBandCold},  // third quartile
		{26, 100, tiering.ActivityBandCold},  // third quartile
		{10, 100, tiering.ActivityBandIdle},  // bottom quartile
		{0, 100, tiering.ActivityBandIdle},   // zero-rate
		{100, 0, tiering.ActivityBandIdle},   // zero top-percentile
	}

	for _, tc := range tests {
		got := classifyMdadmBand(tc.regionIOPS, tc.topIOPS)
		if got != tc.wantBand {
			t.Errorf("classifyMdadmBand(regionIOPS=%v, topIOPS=%v) = %q, want %q",
				tc.regionIOPS, tc.topIOPS, got, tc.wantBand)
		}
	}
}

// TestZFSActivityBandDerivationRules documents the managed ZFS band derivation
// rules from the proposal and verifies a reference implementation.
//
// The proposal defines:
//   - Hot  = accessed more than once per hour
//   - Warm = accessed once per day
//   - Cold = accessed once per week
//   - Idle = not accessed within the collection window
func TestZFSActivityBandDerivationRules(t *testing.T) {
	const (
		hour = 60.0          // minutes
		day  = 24 * 60.0     // minutes
		week = 7 * 24 * 60.0 // minutes
	)

	// classifyZFSBand takes the access frequency in accesses-per-minute and
	// returns the appropriate band per the proposal definition.
	//
	// Boundary interpretation: "once per day" means ≥ 1/day (inclusive).
	// "more than once per hour" is strictly greater.
	classifyZFSBand := func(accessesPerMinute float64) string {
		const (
			oncePerHour = 1.0 / hour
			oncePerDay  = 1.0 / day
			oncePerWeek = 1.0 / week
		)
		switch {
		case accessesPerMinute > oncePerHour:
			return tiering.ActivityBandHot
		case accessesPerMinute >= oncePerDay:
			return tiering.ActivityBandWarm
		case accessesPerMinute >= oncePerWeek:
			return tiering.ActivityBandCold
		default:
			return tiering.ActivityBandIdle
		}
	}

	tests := []struct {
		accessesPerMinute float64
		wantBand          string
	}{
		{2.0 / hour, tiering.ActivityBandHot},        // 2×/hr > 1×/hr → hot
		{1.5 / hour, tiering.ActivityBandHot},        // 1.5×/hr → hot
		{1.0 / (2 * hour), tiering.ActivityBandWarm}, // 1×/2hr → warm
		{1.0 / day, tiering.ActivityBandWarm},        // exactly 1×/day → warm (inclusive)
		{0.5 / day, tiering.ActivityBandCold},        // 0.5×/day → cold
		{1.0 / week, tiering.ActivityBandCold},       // exactly 1×/week → cold (inclusive)
		{0.5 / week, tiering.ActivityBandIdle},       // below 1×/week → idle
		{0, tiering.ActivityBandIdle},                // no access → idle
	}

	for _, tc := range tests {
		got := classifyZFSBand(tc.accessesPerMinute)
		if got != tc.wantBand {
			t.Errorf("classifyZFSBand(%v accesses/min) = %q, want %q",
				tc.accessesPerMinute, got, tc.wantBand)
		}
	}
}
