package schedule

import (
	"testing"
	"time"
)

func TestSlotsAndDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, now, clock string
		days             int
		last, want       string
		due              bool
	}{
		{"normal", "2026-11-02T03:00:00+01:00", "02:00", 127, "", "2026-11-02T01:00:00Z", true},
		{"weekday", "2026-11-03T01:00:00+01:00", "02:00", 1, "", "2026-11-02T01:00:00Z", true},
		{"old", "2026-11-03T03:00:00+01:00", "02:00", 1, "", "", false},
		{"claimed", "2026-11-02T03:00:00+01:00", "02:00", 127, "2026-11-02T01:00:00Z", "", false},
		{"spring", "2026-03-29T03:01:00+02:00", "02:30", 127, "", "2026-03-29T01:00:00Z", true},
		{"fall first", "2026-10-25T02:45:00+02:00", "02:30", 127, "", "2026-10-25T00:30:00Z", true},
		{"fall twice", "2026-10-25T02:45:00+01:00", "02:30", 127, "2026-10-25T00:30:00Z", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			slot, ok := Due(now.In(loc), tt.clock, tt.days, tt.last)
			if ok != tt.due {
				t.Fatalf("due=%v slot=%v", ok, slot)
			}
			if ok && slot.UTC().Format(time.RFC3339) != tt.want {
				t.Fatalf("slot=%s want=%s", slot.UTC().Format(time.RFC3339), tt.want)
			}
		})
	}
	now := time.Date(2026, 10, 25, 2, 45, 0, 0, loc)
	if next := Next(now, "02:30", 127); next != "2026-10-26T01:30:00Z" {
		t.Fatalf("next=%s", next)
	}
}
func TestValidation(t *testing.T) {
	for _, clock := range []string{"2:00", "24:00", "23:60", "aa:bb", "02:00x", "-1:20"} {
		if Valid(clock, 127) {
			t.Errorf("accepted %q", clock)
		}
	}
	if Valid("02:00", 0) || Valid("02:00", 128) || !Valid("23:59", 127) {
		t.Fatal("weekday validation")
	}
}
