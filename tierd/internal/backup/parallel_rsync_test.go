package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestEnumerateRsyncWorkUnitsMixedTree covers the typical NAS shape:
// top-level dirs, some with subdirs, some files-only, plus a top-level
// file. Asserts that subdirs become per-subdir units, files-only dirs
// stay whole, and empty dirs become their own unit.
func TestEnumerateRsyncWorkUnitsMixedTree(t *testing.T) {
	root := t.TempDir()
	must := func(p string, isDir bool) {
		t.Helper()
		full := filepath.Join(root, p)
		if isDir {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", full, err)
			}
			return
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir parent of %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	// Top-level file.
	must("toplevel.txt", false)
	// dir-with-subs/{sub-a, sub-b} — emits one unit per subdir.
	must("dir-with-subs/sub-a", true)
	must("dir-with-subs/sub-b", true)
	// files-only-dir/{f1, f2} — emits the dir itself, NOT per file.
	must("files-only-dir/f1", false)
	must("files-only-dir/f2", false)
	// empty-dir/ — emits as a single empty unit so rsync creates it.
	must("empty-dir", true)
	// mixed-dir/{sub-c, file3} — has at least one subdir → recurse;
	// file3 still emitted as its own unit.
	must("mixed-dir/sub-c", true)
	must("mixed-dir/file3", false)

	got, err := enumerateRsyncWorkUnits(root)
	if err != nil {
		t.Fatalf("enumerateRsyncWorkUnits: %v", err)
	}
	rels := make([]string, len(got))
	for i, u := range got {
		rels[i] = u.rel
	}
	sort.Strings(rels)
	want := []string{
		"dir-with-subs/sub-a",
		"dir-with-subs/sub-b",
		"empty-dir",
		"files-only-dir",
		"mixed-dir/file3",
		"mixed-dir/sub-c",
		"toplevel.txt",
	}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("units = %v\nwant   %v", rels, want)
	}
}

// TestEnumerateRsyncWorkUnitsFilesOnlyRoot exercises the tests/synth
// fallback branch: when the source root itself contains no
// subdirectories, the enumerator still has to produce >1 unit so the
// parallel runner doesn't collapse to single-stream.
func TestEnumerateRsyncWorkUnitsFilesOnlyRoot(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got, err := enumerateRsyncWorkUnits(root)
	if err != nil {
		t.Fatalf("enumerateRsyncWorkUnits: %v", err)
	}
	rels := make([]string, len(got))
	for i, u := range got {
		rels[i] = u.rel
	}
	sort.Strings(rels)
	want := []string{"a.bin", "b.bin", "c.bin"}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("units = %v, want %v", rels, want)
	}
}

// TestEnumerateRsyncWorkUnitsEmptyRoot — empty source produces zero
// units, and the runner short-circuits cleanly above this layer.
func TestEnumerateRsyncWorkUnitsEmptyRoot(t *testing.T) {
	got, err := enumerateRsyncWorkUnits(t.TempDir())
	if err != nil {
		t.Fatalf("enumerateRsyncWorkUnits: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty root should produce 0 units, got %v", got)
	}
}
