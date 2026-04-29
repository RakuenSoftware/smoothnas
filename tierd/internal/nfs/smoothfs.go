// Smoothfs export helpers. Per Phase 0 contract §0.7 Phase 4 addenda,
// every smoothfs-backed NFS handle carries fsid information at two
// layers — nfsd's per-export fsid (set in /etc/exports) for routing,
// and smoothfs's xxh32(pool_uuid) prefix inside the fileid body for
// per-fs sanity-check on decode. Both are derived from pool_uuid here
// so the two cannot drift.

package nfs

import (
	"path/filepath"

	"github.com/google/uuid"
)

// SmoothfsFsidOption renders the value for /etc/exports' fsid= option
// for a smoothfs pool. exportfs(8) accepts integer, "uuid", or a
// UUID string; we use the UUID form since it's stable across reboots
// and ties one-to-one to the pool.
func SmoothfsFsidOption(poolUUID uuid.UUID) string {
	return poolUUID.String()
}

// SmoothfsExportFsidOption returns a stable fsid= value for an export inside a
// smoothfs pool. The pool root keeps the pool UUID; subpath exports need their
// own stable fsid because exportfs requires one for every smoothfs export.
func SmoothfsExportFsidOption(poolUUID uuid.UUID, mountpoint, exportPath string) string {
	mountpoint = filepath.Clean(mountpoint)
	exportPath = filepath.Clean(exportPath)
	if exportPath == mountpoint {
		return SmoothfsFsidOption(poolUUID)
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("smoothfs-nfs-export\x00"+poolUUID.String()+"\x00"+exportPath)).String()
}

// SmoothfsFileidFsidPrefix returns the 32-bit value smoothfs's kernel
// encode_fh writes as the leading 4 bytes of the 24-byte fileid body.
// Used here only as a regression check (the kernel computes the same
// value via xxh32 in smoothfs_fill_super); not written into
// /etc/exports — that's what SmoothfsFsidOption is for.
//
// Bit-exact agreement with smoothfs/super.c:
//
//	sbi->fsid = xxh32(sbi->pool_uuid.b, sizeof(sbi->pool_uuid.b), 0);
func SmoothfsFileidFsidPrefix(poolUUID uuid.UUID) uint32 {
	return xxh32(poolUUID[:], 0)
}

// BuildSmoothfsExport composes an Export for a smoothfs-backed
// mountpoint. Sets fsid= from the pool UUID. Defaults match the
// Phase 4 plan: rw, sync (placement-log fsync ordering depends on
// it), no_root_squash. Callers can override fields on the returned
// Export before passing to GenerateExports / WriteExports.
func BuildSmoothfsExport(mountpoint string, poolUUID uuid.UUID, networks []string) Export {
	return Export{
		Path:     mountpoint,
		Networks: networks,
		Sync:     true,
		ReadOnly: false,
		Fsid:     SmoothfsExportFsidOption(poolUUID, mountpoint, mountpoint),
	}
}

// xxh32 is a minimal in-tree XXH32 implementation. Mirrors Linux's
// lib/xxhash.c xxh32() — same primes, same mixing, little-endian
// input reads. Inlined rather than pulling a third-party dep because
// (a) we need bit-exact agreement with one specific kernel function
// and (b) the algorithm is ~30 lines.
//
// XXH32 reference: https://github.com/Cyan4973/xxHash (BSD-2).
func xxh32(data []byte, seed uint32) uint32 {
	const (
		prime1 uint32 = 0x9E3779B1
		prime2 uint32 = 0x85EBCA77
		prime3 uint32 = 0xC2B2AE3D
		prime4 uint32 = 0x27D4EB2F
		prime5 uint32 = 0x165667B1
	)
	rol := func(x uint32, n uint) uint32 { return (x << n) | (x >> (32 - n)) }
	round := func(state, input uint32) uint32 {
		return rol(state+input*prime2, 13) * prime1
	}
	le := func(p []byte) uint32 {
		return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24
	}

	var h32 uint32
	length := uint32(len(data))
	p := data

	if length >= 16 {
		v1 := seed + prime1 + prime2
		v2 := seed + prime2
		v3 := seed
		v4 := seed - prime1
		for len(p) >= 16 {
			v1 = round(v1, le(p[0:4]))
			v2 = round(v2, le(p[4:8]))
			v3 = round(v3, le(p[8:12]))
			v4 = round(v4, le(p[12:16]))
			p = p[16:]
		}
		h32 = rol(v1, 1) + rol(v2, 7) + rol(v3, 12) + rol(v4, 18)
	} else {
		h32 = seed + prime5
	}

	h32 += length

	for len(p) >= 4 {
		h32 += le(p[0:4]) * prime3
		h32 = rol(h32, 17) * prime4
		p = p[4:]
	}
	for _, b := range p {
		h32 += uint32(b) * prime5
		h32 = rol(h32, 11) * prime1
	}

	h32 ^= h32 >> 15
	h32 *= prime2
	h32 ^= h32 >> 13
	h32 *= prime3
	h32 ^= h32 >> 16
	return h32
}
