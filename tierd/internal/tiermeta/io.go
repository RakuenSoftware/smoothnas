package tiermeta

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
)

// Binary envelope written at the start of every metadata LV.
//
//	[4 B] magic   0x534D4554  ("SMET")
//	[4 B] version uint32 LE
//	[8 B] length  uint64 LE  (byte count of JSON payload)
//	[N B] JSON payload
const (
	metaMagic   uint32 = 0x534D4554
	headerSize         = 4 + 4 + 8 // magic + version + length
)

// WriteTierMeta serialises meta and writes it to the per-tier metadata LV
// ("tiermeta") inside the given tier VG (e.g. "tier-media-NVME").
func WriteTierMeta(vg string, meta *TierMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal tier meta: %w", err)
	}
	return writeMeta(DevicePath(vg, TierLVName), data)
}

// ReadTierMeta reads and deserialises TierMeta from the per-tier metadata LV.
func ReadTierMeta(vg string) (*TierMeta, error) {
	data, err := readMeta(DevicePath(vg, TierLVName))
	if err != nil {
		return nil, err
	}
	var meta TierMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal tier meta: %w", err)
	}
	return &meta, nil
}

// WriteCompleteMeta serialises pool and writes it to the complete-metadata LV.
func WriteCompleteMeta(vg string, pool *PoolMeta) error {
	data, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("marshal complete meta: %w", err)
	}
	return writeMeta(DevicePath(vg, CompleteLVName), data)
}

// ReadCompleteMeta reads and deserialises the PoolMeta from the complete LV.
func ReadCompleteMeta(vg string) (*PoolMeta, error) {
	data, err := readMeta(DevicePath(vg, CompleteLVName))
	if err != nil {
		return nil, err
	}
	var pool PoolMeta
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, fmt.Errorf("unmarshal complete meta: %w", err)
	}
	return &pool, nil
}

// WriteSlotMeta serialises slot as a TierMeta and writes it to the per-tier
// metadata LV.  Kept for backward compatibility; prefer WriteTierMeta.
func WriteSlotMeta(vg, slotName string, slot *SlotMeta) error {
	tm := &TierMeta{
		Version:   MetaVersion,
		PoolName:  slot.PoolName,
		Filesystem: "xfs",
		PoolState: SlotStateAssigned,
		Slot:      *slot,
		UpdatedAt: slot.UpdatedAt,
	}
	return WriteTierMeta(vg, tm)
}

// ReadSlotMeta reads the TierMeta LV and returns just the SlotMeta portion.
// Kept for backward compatibility; prefer ReadTierMeta.
func ReadSlotMeta(vg, _ string) (*SlotMeta, error) {
	tm, err := ReadTierMeta(vg)
	if err != nil {
		return nil, err
	}
	return &tm.Slot, nil
}

// writeMeta writes a binary envelope containing payload to the raw block
// device at devicePath.  The file is opened O_WRONLY|O_SYNC so writes are
// durable without an explicit fsync.
func writeMeta(devicePath string, payload []byte) error {
	f, err := os.OpenFile(devicePath, os.O_WRONLY|os.O_SYNC, 0)
	if err != nil {
		return fmt.Errorf("open %s for write: %w", devicePath, err)
	}
	defer f.Close()

	hdr := make([]byte, headerSize)
	binary.LittleEndian.PutUint32(hdr[0:4], metaMagic)
	binary.LittleEndian.PutUint32(hdr[4:8], MetaVersion)
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(len(payload)))

	if _, err := f.Write(hdr); err != nil {
		return fmt.Errorf("write header to %s: %w", devicePath, err)
	}
	if _, err := f.Write(payload); err != nil {
		return fmt.Errorf("write payload to %s: %w", devicePath, err)
	}
	return nil
}

// readMeta reads and validates the binary envelope from devicePath and returns
// the raw JSON payload bytes.
func readMeta(devicePath string) ([]byte, error) {
	f, err := os.Open(devicePath)
	if err != nil {
		return nil, fmt.Errorf("open %s for read: %w", devicePath, err)
	}
	defer f.Close()

	hdr := make([]byte, headerSize)
	if _, err := readFull(f, hdr); err != nil {
		return nil, fmt.Errorf("read header from %s: %w", devicePath, err)
	}

	magic := binary.LittleEndian.Uint32(hdr[0:4])
	if magic != metaMagic {
		return nil, fmt.Errorf("%s: invalid magic 0x%08X (want 0x%08X)", devicePath, magic, metaMagic)
	}
	// Version is informational for now; future versions may add migration logic.
	length := binary.LittleEndian.Uint64(hdr[8:16])
	if length == 0 {
		return nil, fmt.Errorf("%s: metadata is empty", devicePath)
	}
	if length > 64*1024*1024 { // sanity cap: 64 MiB
		return nil, fmt.Errorf("%s: metadata length %d exceeds sanity cap", devicePath, length)
	}

	payload := make([]byte, length)
	if _, err := readFull(f, payload); err != nil {
		return nil, fmt.Errorf("read payload from %s: %w", devicePath, err)
	}
	return payload, nil
}

// readFull reads exactly len(buf) bytes from r into buf.
func readFull(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
