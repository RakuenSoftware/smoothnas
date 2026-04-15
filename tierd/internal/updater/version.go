package updater

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var testingVersionPattern = regexp.MustCompile(`^(?:testing-)?\d{4}\.\d{4}\.\d{4}(?:-[0-9A-Za-z]+)?$`)

// compareVersions returns true if latest is newer than current.
//
// Supported formats:
//   - semver:         0.1.0 / v0.2.0
//   - date+time:      2026.0405.1423-abc1234   (YYYY.MMDD.HHMM-sha)
//   - testing-prefix: testing-2026.0405.1423-abc1234
func compareVersions(current, latest string) (bool, error) {
	cur, err := parseVersion(current)
	if err != nil {
		return false, fmt.Errorf("parse current version: %w", err)
	}
	lat, err := parseVersion(latest)
	if err != nil {
		return false, fmt.Errorf("parse latest version: %w", err)
	}

	// If the two versions use different schemes (calendar vs semver), treat the
	// latest as always available. This covers switching from a date-based testing
	// build (e.g. 2026.0410.2058) to a semver stable release (e.g. 0.4.2), or
	// vice-versa, where the numeric comparison would otherwise be meaningless.
	if isCalendarVersion(cur) != isCalendarVersion(lat) {
		return true, nil
	}

	if lat[0] != cur[0] {
		return lat[0] > cur[0], nil
	}
	if lat[1] != cur[1] {
		return lat[1] > cur[1], nil
	}
	return lat[2] > cur[2], nil
}

// isCalendarVersion reports whether the parsed version looks like a date-based
// build stamp (year in the major component, e.g. 2026.0410.2058).
func isCalendarVersion(v [3]int) bool {
	return v[0] >= 2000
}

func parseVersion(v string) ([3]int, error) {
	v = normalizeReleaseVersion(v)

	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return [3]int{}, fmt.Errorf("invalid version %q: expected major.minor.patch", v)
	}

	var result [3]int
	for i, p := range parts {
		// Strip any suffix after a hyphen (e.g. "1423-abc1234" → "1423").
		if idx := strings.IndexByte(p, '-'); idx >= 0 {
			p = p[:idx]
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, fmt.Errorf("invalid version component %q: %w", parts[i], err)
		}
		result[i] = n
	}
	return result, nil
}

func normalizeReleaseVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	return strings.TrimPrefix(v, "testing-")
}

func defaultChannelForVersion(version string) Channel {
	if testingVersionPattern.MatchString(version) {
		return ChannelTesting
	}
	return ChannelStable
}
