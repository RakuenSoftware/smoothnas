// Package schedule computes one daily wall-clock slot in the NAS timezone.
package schedule

import (
	"fmt"
	"time"
)

func Valid(clock string, days int) bool {
	var h, m int
	if len(clock) != 5 || clock[2] != ':' {
		return false
	}
	if _, err := fmt.Sscanf(clock, "%02d:%02d", &h, &m); err != nil {
		return false
	}
	return fmt.Sprintf("%02d:%02d", h, m) == clock && h >= 0 && h < 24 && m >= 0 && m < 60 && days > 0 && days <= 127
}

// Slots enumerates real minutes so a missing DST time advances to the first
// valid minute and a repeated time uses its first occurrence only.
func slot(day time.Time, clock string) time.Time {
	var h, m int
	fmt.Sscanf(clock, "%02d:%02d", &h, &m)
	target := h*60 + m
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	end := start.AddDate(0, 0, 1)
	for t := start; t.Before(end); t = t.Add(time.Minute) {
		if t.Hour()*60+t.Minute() >= target {
			return t
		}
	}
	return end
}
func selected(day time.Time, days int) bool { return days&(1<<((int(day.Weekday())+6)%7)) != 0 }
func Due(now time.Time, clock string, days int, last string) (time.Time, bool) {
	if !Valid(clock, days) {
		return time.Time{}, false
	}
	previous, _ := time.Parse(time.RFC3339, last)
	for i := 0; i < 3; i++ {
		day := now.AddDate(0, 0, -i)
		if !selected(day, days) {
			continue
		}
		s := slot(day, clock)
		if !s.After(now) && now.Sub(s) <= 24*time.Hour && s.After(previous) {
			return s, true
		}
	}
	return time.Time{}, false
}
func Next(now time.Time, clock string, days int) string {
	if !Valid(clock, days) {
		return ""
	}
	for i := 0; i < 8; i++ {
		day := now.AddDate(0, 0, i)
		if selected(day, days) {
			s := slot(day, clock)
			if s.After(now) {
				return s.UTC().Format(time.RFC3339)
			}
		}
	}
	return ""
}
