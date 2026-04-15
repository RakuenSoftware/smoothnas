package tiermeta

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tempDevice creates a temporary regular file sized to hold a header + plenty
// of payload.  Regular files work the same as raw block devices for our I/O
// code because we only use os.OpenFile + sequential reads/writes.
func tempDevice(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "meta_lv_*")
	if err != nil {
		t.Fatalf("create temp device: %v", err)
	}
	// Pre-allocate 1 MiB.
	if err := f.Truncate(1 << 20); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()
	return f.Name()
}

// writeRaw writes a binary envelope at path without going through the public
// API, so we can test individual error cases.
func writeRaw(t *testing.T, path string, magic, version uint32, payload []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for write: %v", err)
	}
	defer f.Close()

	hdr := make([]byte, headerSize)
	binary.LittleEndian.PutUint32(hdr[0:4], magic)
	binary.LittleEndian.PutUint32(hdr[4:8], version)
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(len(payload)))
	if _, err := f.Write(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := f.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

// --- writeMeta / readMeta round-trips ---

func TestWriteReadMetaRoundTrip(t *testing.T) {
	dev := tempDevice(t)
	payload := []byte(`{"hello":"world"}`)

	if err := writeMeta(dev, payload); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}
	got, err := readMeta(dev)
	if err != nil {
		t.Fatalf("readMeta: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("payload mismatch: got %q, want %q", got, payload)
	}
}

func TestWriteReadMetaLargePayload(t *testing.T) {
	dev := tempDevice(t)
	// 512 KiB of 'x'
	payload := make([]byte, 512*1024)
	for i := range payload {
		payload[i] = 'x'
	}
	if err := writeMeta(dev, payload); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}
	got, err := readMeta(dev)
	if err != nil {
		t.Fatalf("readMeta: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("length mismatch: got %d, want %d", len(got), len(payload))
	}
}

func TestReadMetaRejectsWrongMagic(t *testing.T) {
	dev := tempDevice(t)
	writeRaw(t, dev, 0xDEADBEEF, MetaVersion, []byte(`{}`))
	_, err := readMeta(dev)
	if err == nil {
		t.Fatal("expected error for wrong magic")
	}
}

func TestReadMetaRejectsEmptyPayload(t *testing.T) {
	dev := tempDevice(t)
	writeRaw(t, dev, metaMagic, MetaVersion, []byte{})
	_, err := readMeta(dev)
	if err == nil {
		t.Fatal("expected error for zero-length payload")
	}
}

func TestReadMetaNonExistentFile(t *testing.T) {
	_, err := readMeta(filepath.Join(t.TempDir(), "no_such_lv"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// --- SlotMeta round-trip through writeMeta / readMeta + json.Marshal ---

func TestSlotMetaRoundTrip(t *testing.T) {
	dev := tempDevice(t)
	now := time.Now().UTC().Truncate(time.Second)
	original := SlotMeta{
		Version:   MetaVersion,
		PoolName:  "test",
		SlotName:  SlotHDD,
		Rank:      3,
		State:     SlotStateAssigned,
		ArrayPath: "/dev/md0",
		PVDevice:  "/dev/md0",
		CreatedAt: now,
		UpdatedAt: now,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := writeMeta(dev, data); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}
	raw, err := readMeta(dev)
	if err != nil {
		t.Fatalf("readMeta: %v", err)
	}
	var got SlotMeta
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SlotName != original.SlotName || got.ArrayPath != original.ArrayPath || got.Rank != original.Rank {
		t.Errorf("slot mismatch: got %+v, want %+v", got, original)
	}
}

// --- PoolMeta round-trip through writeMeta / readMeta + json.Marshal ---

func TestPoolMetaRoundTrip(t *testing.T) {
	dev := tempDevice(t)
	now := time.Now().UTC().Truncate(time.Second)
	original := PoolMeta{
		Version:    MetaVersion,
		Name:       "media",
		Filesystem: "xfs",
		State:      PoolStateHealthy,
		CreatedAt:  now,
		UpdatedAt:  now,
		Slots: []SlotMeta{
			{SlotName: SlotNVME, Rank: 1, State: SlotStateAssigned, ArrayPath: "/dev/md1"},
			{SlotName: SlotHDD, Rank: 3, State: SlotStateAssigned, ArrayPath: "/dev/md0"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := writeMeta(dev, data); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}
	raw, err := readMeta(dev)
	if err != nil {
		t.Fatalf("readMeta: %v", err)
	}
	var got PoolMeta
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != original.Name || got.State != original.State || len(got.Slots) != len(original.Slots) {
		t.Errorf("pool mismatch: got %+v, want %+v", got, original)
	}
}
