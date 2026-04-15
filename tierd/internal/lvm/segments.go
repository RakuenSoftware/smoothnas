package lvm

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Segment is one PE range from `lvs -o seg_pe_ranges`. PEStart/PEEnd are
// inclusive PE indices on the source PV. The corresponding LV byte offset is
// not stored here — callers translate from PE counts using the VG's PE size.
type Segment struct {
	LVName  string
	VGName  string
	PVPath  string
	PEStart uint64
	PEEnd   uint64
}

// PECount returns the number of physical extents in the segment.
func (s Segment) PECount() uint64 {
	if s.PEEnd < s.PEStart {
		return 0
	}
	return s.PEEnd - s.PEStart + 1
}

// ParseSegPERanges parses the seg_pe_ranges output column from lvs.
//
// The seg_pe_ranges field is a space-separated list of "/dev/path:start-end"
// tokens. Multiple segments may belong to the same LV when an LV spans
// segments or PVs. Example for a single line: "/dev/md0:0-99 /dev/md1:0-49".
//
// This parser is intentionally pure so it is unit-testable without LVM.
func ParseSegPERanges(lvName, vgName, field string) ([]Segment, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return nil, nil
	}
	var out []Segment
	for _, tok := range strings.Fields(field) {
		colon := strings.LastIndex(tok, ":")
		if colon <= 0 || colon == len(tok)-1 {
			return nil, fmt.Errorf("malformed seg_pe_ranges token %q", tok)
		}
		pv := tok[:colon]
		rng := tok[colon+1:]
		dash := strings.Index(rng, "-")
		if dash <= 0 || dash == len(rng)-1 {
			return nil, fmt.Errorf("malformed PE range %q in token %q", rng, tok)
		}
		start, err := strconv.ParseUint(rng[:dash], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse PE start %q: %w", rng[:dash], err)
		}
		end, err := strconv.ParseUint(rng[dash+1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse PE end %q: %w", rng[dash+1:], err)
		}
		if end < start {
			return nil, fmt.Errorf("PE end %d before start %d", end, start)
		}
		out = append(out, Segment{
			LVName:  lvName,
			VGName:  vgName,
			PVPath:  pv,
			PEStart: start,
			PEEnd:   end,
		})
	}
	return out, nil
}

// ListLVSegments returns the segments for a single LV by shelling out to
// `lvs -o seg_pe_ranges`. Returns nil on lvs failure (lvs may be missing in
// non-LVM test environments).
func ListLVSegments(vg, lv string) ([]Segment, error) {
	out, err := exec.Command(
		"lvs",
		"--noheadings", "--nosuffix",
		"-o", "seg_pe_ranges",
		vg+"/"+lv,
	).Output()
	if err != nil {
		return nil, nil
	}
	field := strings.TrimSpace(string(out))
	return ParseSegPERanges(lv, vg, field)
}

// VGExtentSizeBytes returns the PE size of a VG. Used to convert region byte
// offsets to PE indices when scoping pvmove. Returns 0 + nil on lookup
// failure so callers can fall back to defaults in tests.
func VGExtentSizeBytes(vg string) (uint64, error) {
	out, err := exec.Command(
		"vgs", "--noheadings", "--nosuffix", "--units", "b",
		"-o", "vg_extent_size", vg,
	).Output()
	if err != nil {
		return 0, nil
	}
	return parseUint(string(out)), nil
}
