// Package meta stores per-object tier metadata on the pool's fastest tier.
//
// The store is designed for the FUSE hot path: synchronous reads at mmap
// latency, and writes that enqueue into a per-shard batched writer so the
// caller never waits on disk. The file that a record describes always lives
// in a tier's backing filesystem — the record itself is a cache/hint that
// can be rebuilt by walking the backing tree if lost.
package meta

import (
	"encoding/binary"
	"fmt"
)

// RecordSize is the fixed on-disk length of every value in the objects DB.
// Keeping it fixed lets us scan shards cheaply and avoids per-record length
// prefixes.
const RecordSize = 32

// RecordVersion is the current schema version written into every record.
// Readers reject any version they don't understand.
const RecordVersion uint16 = 1

// PinState is a small enum matching the user-facing pin states.
type PinState uint8

const (
	PinNone PinState = 0
	PinHot  PinState = 1
	PinCold PinState = 2
)

// Record is the fixed 32-byte per-object metadata stored under an inode key.
//
// Layout (little-endian):
//
//	[0..2)    version        uint16
//	[2..3)    pin_state      uint8
//	[3..4)    tier_idx       uint8   (rank of the tier currently holding the file)
//	[4..12)   namespace_id   uint64  (xxhash64 of namespace string)
//	[12..16)  heat_counter   uint32  (placeholder; zero until heat is re-enabled)
//	[16..24)  last_access_ns uint64  (placeholder)
//	[24..32)  reserved       uint64
type Record struct {
	Version      uint16
	PinState     PinState
	TierIdx      uint8
	NamespaceID  uint64
	HeatCounter  uint32
	LastAccessNS uint64
}

// Encode writes the record into a freshly-allocated RecordSize-byte slice.
func (r Record) Encode() []byte {
	b := make([]byte, RecordSize)
	r.EncodeInto(b)
	return b
}

// EncodeInto writes the record into an existing slice. The slice must be at
// least RecordSize bytes.
func (r Record) EncodeInto(b []byte) {
	_ = b[RecordSize-1] // bounds check hint
	binary.LittleEndian.PutUint16(b[0:2], r.Version)
	b[2] = uint8(r.PinState)
	b[3] = r.TierIdx
	binary.LittleEndian.PutUint64(b[4:12], r.NamespaceID)
	binary.LittleEndian.PutUint32(b[12:16], r.HeatCounter)
	binary.LittleEndian.PutUint64(b[16:24], r.LastAccessNS)
	// reserved[24..32) left zero
}

// DecodeRecord parses a RecordSize-byte value. Returns an error if the value
// is the wrong length or uses an unknown schema version.
func DecodeRecord(b []byte) (Record, error) {
	if len(b) != RecordSize {
		return Record{}, fmt.Errorf("meta: record length %d, want %d", len(b), RecordSize)
	}
	version := binary.LittleEndian.Uint16(b[0:2])
	if version != RecordVersion {
		return Record{}, fmt.Errorf("meta: unknown record version %d", version)
	}
	return Record{
		Version:      version,
		PinState:     PinState(b[2]),
		TierIdx:      b[3],
		NamespaceID:  binary.LittleEndian.Uint64(b[4:12]),
		HeatCounter:  binary.LittleEndian.Uint32(b[12:16]),
		LastAccessNS: binary.LittleEndian.Uint64(b[16:24]),
	}, nil
}

// InodeKey renders an inode as an 8-byte big-endian key. Big-endian so that
// numeric inode ordering matches byte ordering in bbolt cursors — useful for
// debugging but not load-bearing.
func InodeKey(inode uint64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, inode)
	return k
}
