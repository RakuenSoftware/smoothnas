package spindown

import (
	"testing"
	"time"
)

func TestActiveWindowDailyAndOvernight(t *testing.T) {
	windows, err := NormalizeWindows([]ActiveWindow{{
		Days:  []string{"daily"},
		Start: "22:00",
		End:   "06:00",
	}})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !IsActive(windows, time.Date(2026, 4, 20, 23, 30, 0, 0, time.UTC)) {
		t.Fatal("expected late same-day time to be active")
	}
	if !IsActive(windows, time.Date(2026, 4, 21, 2, 0, 0, 0, time.UTC)) {
		t.Fatal("expected overnight time to be active")
	}
	if IsActive(windows, time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("expected midday to be inactive")
	}
}

func TestNextActive(t *testing.T) {
	windows, err := NormalizeWindows([]ActiveWindow{{
		Days:  []string{"mon"},
		Start: "03:00",
		End:   "04:00",
	}})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	next, ok := NextActive(windows, time.Date(2026, 4, 19, 23, 59, 30, 0, time.UTC))
	if !ok {
		t.Fatal("expected next active time")
	}
	want := time.Date(2026, 4, 20, 3, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next active = %s, want %s", next, want)
	}
}

func TestNormalizeRejectsBadWindow(t *testing.T) {
	if _, err := NormalizeWindows([]ActiveWindow{{Days: []string{"funday"}, Start: "01:00", End: "02:00"}}); err == nil {
		t.Fatal("expected invalid day to fail")
	}
	if _, err := NormalizeWindows([]ActiveWindow{{Days: []string{"daily"}, Start: "02:00", End: "02:00"}}); err == nil {
		t.Fatal("expected zero-length window to fail")
	}
}
