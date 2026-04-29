package spindown

import (
	"testing"
	"time"
)

func TestPowerObserverTracksStandbyAndWake(t *testing.T) {
	now := time.Date(2026, 4, 20, 1, 0, 0, 0, time.UTC)
	obs := NewPowerObserver()
	obs.now = func() time.Time { return now }

	obs.Observe("/dev/sda", "standby", "")
	now = now.Add(30 * time.Minute)
	got := obs.Observe("/dev/sda", "active", "operator read")

	if got.ObservedStandbySeconds != int64((30 * time.Minute).Seconds()) {
		t.Fatalf("standby seconds = %d", got.ObservedStandbySeconds)
	}
	if got.LastWakeReason != "operator read" || got.LastWakeAt == "" {
		t.Fatalf("unexpected wake attribution: %+v", got)
	}
	if len(got.RecentWakeEvents) != 1 || got.RecentWakeEvents[0].FromState != "standby" {
		t.Fatalf("unexpected events: %+v", got.RecentWakeEvents)
	}
}

func TestPowerObserverManualEvent(t *testing.T) {
	now := time.Date(2026, 4, 20, 1, 0, 0, 0, time.UTC)
	obs := NewPowerObserver()
	obs.now = func() time.Time { return now }

	got := obs.RecordEvent("/dev/sda", "active", "standby", "manual standby")
	if len(got.RecentWakeEvents) != 1 {
		t.Fatalf("expected manual event: %+v", got)
	}
	if got.RecentWakeEvents[0].Reason != "manual standby" {
		t.Fatalf("reason = %q", got.RecentWakeEvents[0].Reason)
	}
}
