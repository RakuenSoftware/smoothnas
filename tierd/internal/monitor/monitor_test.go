package monitor

import (
	"testing"
	"time"
)

func TestNewMonitor(t *testing.T) {
	m := New(nil, nil)
	if m == nil {
		t.Fatal("expected non-nil monitor")
	}
	if m.pollInterval != 30*time.Second {
		t.Errorf("expected 30s poll interval, got %v", m.pollInterval)
	}
}

func TestAlertLifecycle(t *testing.T) {
	m := New(nil, nil)

	// No alerts initially.
	if m.AlertCount() != 0 {
		t.Fatalf("expected 0 alerts, got %d", m.AlertCount())
	}

	// Add an alert.
	m.addAlert(Alert{
		Source:   "test",
		Severity: "warning",
		Message:  "test alert",
		Device:   "/dev/sda",
		Timestamp: time.Now(),
	})

	if m.AlertCount() != 1 {
		t.Fatalf("expected 1 alert, got %d", m.AlertCount())
	}

	alerts := m.GetAlerts()
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert in list, got %d", len(alerts))
	}
	if alerts[0].Message != "test alert" {
		t.Errorf("expected 'test alert', got %q", alerts[0].Message)
	}
	if alerts[0].ID == "" {
		t.Error("alert should have an ID")
	}

	// Clear the alert.
	m.ClearAlert(alerts[0].ID)
	if m.AlertCount() != 0 {
		t.Fatalf("expected 0 alerts after clear, got %d", m.AlertCount())
	}
}

func TestAlertDeduplication(t *testing.T) {
	m := New(nil, nil)

	alert := Alert{
		Source:   "smart",
		Severity: "warning",
		Message:  "Reallocated_Sector_Ct = 5",
		Device:   "/dev/sda",
		Timestamp: time.Now(),
	}

	m.addAlert(alert)
	m.addAlert(alert) // duplicate
	m.addAlert(alert) // duplicate

	if m.AlertCount() != 1 {
		t.Fatalf("expected 1 alert (deduped), got %d", m.AlertCount())
	}
}

func TestAlertDifferentDevices(t *testing.T) {
	m := New(nil, nil)

	m.addAlert(Alert{Source: "smart", Severity: "warning", Message: "hot", Device: "/dev/sda", Timestamp: time.Now()})
	m.addAlert(Alert{Source: "smart", Severity: "warning", Message: "hot", Device: "/dev/sdb", Timestamp: time.Now()})

	if m.AlertCount() != 2 {
		t.Fatalf("expected 2 alerts (different devices), got %d", m.AlertCount())
	}
}

func TestCleanExpired(t *testing.T) {
	m := New(nil, nil)

	// Add an old alert.
	m.addAlert(Alert{
		Source:   "test",
		Severity: "warning",
		Message:  "old alert",
		Device:   "/dev/sda",
		Timestamp: time.Now().Add(-25 * time.Hour), // older than 24h
	})

	if m.AlertCount() != 1 {
		t.Fatalf("expected 1 alert, got %d", m.AlertCount())
	}

	m.cleanExpired()

	if m.AlertCount() != 0 {
		t.Fatalf("expected 0 alerts after cleanup, got %d", m.AlertCount())
	}
}

func TestCleanExpiredKeepsRecent(t *testing.T) {
	m := New(nil, nil)

	m.addAlert(Alert{Source: "test", Severity: "warning", Message: "recent", Device: "/dev/sda", Timestamp: time.Now()})
	m.addAlert(Alert{Source: "test", Severity: "warning", Message: "old", Device: "/dev/sdb", Timestamp: time.Now().Add(-25 * time.Hour)})

	m.cleanExpired()

	if m.AlertCount() != 1 {
		t.Fatalf("expected 1 alert (recent kept), got %d", m.AlertCount())
	}

	alerts := m.GetAlerts()
	if alerts[0].Message != "recent" {
		t.Errorf("expected recent alert, got %q", alerts[0].Message)
	}
}

func TestStartStop(t *testing.T) {
	m := NewWithIntervals(nil, nil, 100*time.Millisecond, 1*time.Hour)
	m.Start()
	time.Sleep(250 * time.Millisecond)
	m.Stop()
	// Should not panic or hang.
}

func TestGetAlertsReturnsCopy(t *testing.T) {
	m := New(nil, nil)
	m.addAlert(Alert{Source: "test", Severity: "warning", Message: "a", Device: "/dev/sda", Timestamp: time.Now()})

	alerts := m.GetAlerts()
	alerts[0].Message = "modified"

	// Original should be unchanged.
	original := m.GetAlerts()
	if original[0].Message != "a" {
		t.Error("GetAlerts should return a copy, not a reference")
	}
}

func TestFormatInt64(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"}, {1, "1"}, {42, "42"}, {-5, "-5"}, {12345, "12345"},
	}
	for _, tt := range tests {
		got := formatInt64(tt.n)
		if got != tt.want {
			t.Errorf("formatInt64(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

