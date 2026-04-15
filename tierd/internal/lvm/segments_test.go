package lvm

import (
	"reflect"
	"testing"
)

func TestParseSegPERangesSinglePV(t *testing.T) {
	got, err := ParseSegPERanges("data", "smoothnas", "/dev/md0:0-99")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Segment{{LVName: "data", VGName: "smoothnas", PVPath: "/dev/md0", PEStart: 0, PEEnd: 99}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
	if got[0].PECount() != 100 {
		t.Errorf("PECount = %d, want 100", got[0].PECount())
	}
}

func TestParseSegPERangesMultipleSegments(t *testing.T) {
	got, err := ParseSegPERanges("data", "smoothnas", "/dev/md0:0-49 /dev/md1:100-199")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(got))
	}
	if got[0].PVPath != "/dev/md0" || got[0].PEStart != 0 || got[0].PEEnd != 49 {
		t.Errorf("first seg wrong: %+v", got[0])
	}
	if got[1].PVPath != "/dev/md1" || got[1].PEStart != 100 || got[1].PEEnd != 199 {
		t.Errorf("second seg wrong: %+v", got[1])
	}
}

func TestParseSegPERangesEmpty(t *testing.T) {
	got, err := ParseSegPERanges("x", "vg", "")
	if err != nil || got != nil {
		t.Errorf("empty input: got=%v err=%v", got, err)
	}
}

func TestParseSegPERangesMalformed(t *testing.T) {
	bad := []string{
		"/dev/md0",          // no colon
		"/dev/md0:abc",      // no range
		"/dev/md0:5",        // no dash
		"/dev/md0:5-",       // missing end
		"/dev/md0:-5",       // missing start
		"/dev/md0:99-0",     // end before start
		"/dev/md0:foo-bar",  // non-numeric
	}
	for _, in := range bad {
		if _, err := ParseSegPERanges("x", "vg", in); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestBuildPVMoveArgs(t *testing.T) {
	got := BuildPVMoveArgs("/dev/md0", 100, 199, "/dev/md1")
	want := []string{"-i", "1", "/dev/md0:100-199", "/dev/md1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestParsePVMoveLine(t *testing.T) {
	cases := map[string]struct {
		ok      bool
		percent float64
		done    bool
	}{
		"  /dev/md0: Moved: 12.34%":  {true, 12.34, false},
		"  /dev/md0: Moved: 100.00%": {true, 100.0, true},
		"  /dev/md0: Moved: 0.00%":   {true, 0.0, false},
		"unrelated noise line":       {false, 0, false},
		"":                           {false, 0, false},
	}
	for line, want := range cases {
		got, ok := ParsePVMoveLine(line)
		if ok != want.ok {
			t.Errorf("%q: ok=%v want %v", line, ok, want.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.PercentDone != want.percent || got.Done != want.done {
			t.Errorf("%q: got %+v want pct=%v done=%v", line, got, want.percent, want.done)
		}
	}
}
