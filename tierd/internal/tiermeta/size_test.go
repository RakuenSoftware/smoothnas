package tiermeta

import (
	"testing"
)

// roundUpExtent mirrors the rounding logic in MetaLVSizeBytes.
func roundUpExtent(n uint64) uint64 {
	const e = lvmExtentSize
	return ((n + e - 1) / e) * e
}

func TestMetaLVSizeBytes(t *testing.T) {
	const extent = lvmExtentSize // 4 MiB

	tests := []struct {
		name    string
		pvBytes uint64
		want    uint64
	}{
		{name: "zero returns minimum", pvBytes: 0, want: extent},
		{name: "tiny (<1000) returns minimum", pvBytes: 999, want: extent},
		{name: "exactly 1000: tenth=1, rounds up to one extent", pvBytes: 1000, want: extent},
		// 4 GiB: tenth = 4294967296/1000 = 4294967, rounded up = 2 extents = 8 MiB
		{name: "4 GiB rounds up to 2 extents", pvBytes: 4 * 1024 * 1024 * 1024, want: roundUpExtent(4*1024*1024*1024 / 1000)},
		// 8 GiB: tenth = 8589934, rounded up = 3 extents = 12 MiB
		{name: "8 GiB rounds up to 3 extents", pvBytes: 8 * 1024 * 1024 * 1024, want: roundUpExtent(8*1024*1024*1024 / 1000)},
		// 1 TiB
		{name: "1 TiB", pvBytes: 1024 * 1024 * 1024 * 1024, want: roundUpExtent(1024 * 1024 * 1024 * 1024 / 1000)},
		// pvBytes such that tenth == exactly one extent → no rounding needed.
		{name: "exact extent multiple", pvBytes: uint64(extent) * 1000, want: extent},
		// pvBytes such that tenth == extent+1 → rounds up to two extents.
		// tenth = pvBytes/1000 = extent+1 requires pvBytes = (extent+1)*1000.
		{name: "tenth one over an extent", pvBytes: (uint64(extent) + 1) * 1000, want: 2 * extent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MetaLVSizeBytes(tc.pvBytes)
			if got != tc.want {
				t.Errorf("MetaLVSizeBytes(%d) = %d, want %d", tc.pvBytes, got, tc.want)
			}
			if got%extent != 0 {
				t.Errorf("MetaLVSizeBytes(%d) = %d is not extent-aligned", tc.pvBytes, got)
			}
			if got < extent {
				t.Errorf("MetaLVSizeBytes(%d) = %d is below minimum extent", tc.pvBytes, got)
			}
		})
	}
}

func TestCompleteMetaLVSizeBytes(t *testing.T) {
	const extent = lvmExtentSize

	t.Run("nil slice returns minimum", func(t *testing.T) {
		got := CompleteMetaLVSizeBytes(nil)
		if got != extent {
			t.Errorf("got %d, want %d", got, extent)
		}
	})

	t.Run("single size same as MetaLVSizeBytes of that size", func(t *testing.T) {
		pv := uint64(8 * 1024 * 1024 * 1024)
		got := CompleteMetaLVSizeBytes([]uint64{pv})
		want := MetaLVSizeBytes(pv)
		if got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	})

	t.Run("sums before computing 0.1 percent", func(t *testing.T) {
		pv := uint64(4 * 1024 * 1024 * 1024) // 4 GiB each
		got := CompleteMetaLVSizeBytes([]uint64{pv, pv, pv})
		want := MetaLVSizeBytes(3 * pv)
		if got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	})
}

func TestBytesToLVMSize(t *testing.T) {
	tests := []struct {
		n    uint64
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{4194304, "4194304B"},
		{1073741824, "1073741824B"},
	}
	for _, tc := range tests {
		got := BytesToLVMSize(tc.n)
		if got != tc.want {
			t.Errorf("BytesToLVMSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
