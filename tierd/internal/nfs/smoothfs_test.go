package nfs

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestXXH32ReferenceVectors verifies our inline XXH32 against the
// canonical reference vectors from the XXH32 specification. These
// values are well-known across all conforming implementations
// (Cyan4973/xxHash, lib/xxhash.c, every Go/Rust/Python port). If
// these match, the algorithm is correct; if the kernel side also
// implements XXH32 to spec (it does — see lib/xxhash.c), the two
// agree by construction on every input.
func TestXXH32ReferenceVectors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		seed uint32
		want uint32
	}{
		{"empty,seed=0", []byte{}, 0, 0x02CC5D05},
		{"empty,seed=prime", []byte{}, 0x9E3779B1, 0x36B78AE7},
		{"abc,seed=0", []byte("abc"), 0, 0x32D153FF},
	}
	for _, tc := range cases {
		got := xxh32(tc.data, tc.seed)
		if got != tc.want {
			t.Errorf("%s: xxh32 = %#08x, want %#08x", tc.name, got, tc.want)
		}
	}
}

// TestSmoothfsFileidFsidPrefixKnownUUID — bit-exact agreement with the
// kernel's xxh32(pool_uuid.b, 16, 0) for a fixed test UUID. This is
// the "matches the kernel formula" test the §0.7 addenda call for.
//
// Verified by mounting smoothfs with this UUID on the lab kernel
// (6.18.22-smoothnas-lts) and reading /proc/mounts:
//
//	none /tmp/.../mnt smoothfs ...,fsid=0xa090dc21,...
//
// (Note that /proc/mounts shows the SMOOTHFS-internal fsid prefix,
// not the /etc/exports value — exactly what this function returns.)
// If this assertion ever fails, smoothfs's encode_fh and tierd's
// xxh32 have diverged and emitted handles will be rejected by
// fh_to_dentry's prefix sanity check. Hard fail.
func TestSmoothfsFileidFsidPrefixKnownUUID(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	const want = uint32(0xa090dc21)
	got := SmoothfsFileidFsidPrefix(id)
	if got != want {
		t.Errorf("SmoothfsFileidFsidPrefix = %#08x, want %#08x (kernel-verified)", got, want)
	}
}

// TestSmoothfsFsidOptionFormat — the /etc/exports fsid= value is the
// pool UUID rendered as a string. Asserts format and length so we
// don't accidentally start emitting hex / numeric forms that
// exportfs(8) would interpret differently.
func TestSmoothfsFsidOptionFormat(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	got := SmoothfsFsidOption(id)
	const want = "01234567-89ab-cdef-0123-456789abcdef"
	if got != want {
		t.Errorf("SmoothfsFsidOption = %q, want %q", got, want)
	}
	if len(strings.Split(got, "-")) != 5 {
		t.Errorf("expected 5-section UUID, got %q", got)
	}
}

func TestSmoothfsExportFsidOptionForSubpath(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	root := SmoothfsExportFsidOption(id, "/mnt/media", "/mnt/media")
	if root != id.String() {
		t.Fatalf("root export fsid = %q, want pool UUID", root)
	}

	sub := SmoothfsExportFsidOption(id, "/mnt/media", "/mnt/media/storage")
	if sub == "" || sub == id.String() {
		t.Fatalf("subpath export fsid = %q, want stable non-pool UUID", sub)
	}
	if again := SmoothfsExportFsidOption(id, "/mnt/media", "/mnt/media/storage"); again != sub {
		t.Fatalf("subpath fsid is not stable: %q then %q", sub, again)
	}
}

// TestBuildSmoothfsExport — defaults match the Phase 4 plan and the
// fsid= option is set from the pool UUID.
func TestBuildSmoothfsExport(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	exp := BuildSmoothfsExport("/mnt/pool", id, []string{"127.0.0.1"})
	if exp.Path != "/mnt/pool" {
		t.Errorf("Path = %q, want /mnt/pool", exp.Path)
	}
	if !exp.Sync {
		t.Error("expected Sync=true (placement-log fsync ordering)")
	}
	if exp.ReadOnly {
		t.Error("expected ReadOnly=false")
	}
	if exp.Fsid != id.String() {
		t.Errorf("Fsid = %q, want %q", exp.Fsid, id.String())
	}
	opts := BuildOptions(exp)
	if !strings.Contains(opts, "fsid="+id.String()) {
		t.Errorf("opts %q missing fsid=%s", opts, id.String())
	}
}
