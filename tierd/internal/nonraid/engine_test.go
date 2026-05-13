package nonraid

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func tempDevice(t *testing.T, size int64) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dev.img")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open temp device: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate temp device: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestEngineWriteUpdatesTripleParityAndReconstructs(t *testing.T) {
	const size = int64(1 << 20)
	data := []*os.File{
		tempDevice(t, size),
		tempDevice(t, size),
		tempDevice(t, size),
		tempDevice(t, size),
	}
	parity := []*os.File{
		tempDevice(t, size),
		tempDevice(t, size),
		tempDevice(t, size),
	}
	dataDevs := make([]BlockDevice, len(data))
	dataSizes := make([]int64, len(data))
	for i := range data {
		dataDevs[i] = data[i]
		dataSizes[i] = size
	}
	parityDevs := make([]BlockDevice, len(parity))
	for i := range parity {
		parityDevs[i] = parity[i]
	}

	engine, err := NewEngine(dataDevs, dataSizes, parityDevs, size)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	writes := []struct {
		slot int
		off  int64
		len  int
	}{
		{slot: 0, off: 0, len: 8192},
		{slot: 1, off: 4096, len: 12000},
		{slot: 2, off: 128 * 1024, len: 65536},
		{slot: 3, off: 7777, len: 32768},
		{slot: 1, off: 7777, len: 8192},
	}
	expected := make([][]byte, len(data))
	for i := range expected {
		expected[i] = make([]byte, size)
	}
	for _, write := range writes {
		payload := make([]byte, write.len)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("random payload: %v", err)
		}
		if err := engine.WriteData(write.slot, write.off, payload); err != nil {
			t.Fatalf("WriteData slot %d: %v", write.slot+1, err)
		}
		start := int(write.off)
		copy(expected[write.slot][start:start+write.len], payload)
	}

	for missingCount := 1; missingCount <= 3; missingCount++ {
		missing := []int{0, 1, 3}[:missingCount]
		for _, slot := range missing {
			got, err := engine.ReconstructData(slot, missing, 0, int(size))
			if err != nil {
				t.Fatalf("ReconstructData slot %d missing %v: %v", slot+1, missing, err)
			}
			if !bytes.Equal(got, expected[slot]) {
				t.Fatalf("reconstructed slot %d with %d missing disks does not match original", slot+1, missingCount)
			}
		}
	}
}

func TestEngineBuildParityFromExistingData(t *testing.T) {
	const size = int64(256 << 10)
	data := []*os.File{tempDevice(t, size), tempDevice(t, size), tempDevice(t, size)}
	parity := []*os.File{tempDevice(t, size), tempDevice(t, size)}

	expected := make([][]byte, len(data))
	for i, dev := range data {
		expected[i] = make([]byte, size)
		if _, err := rand.Read(expected[i]); err != nil {
			t.Fatalf("random data: %v", err)
		}
		if _, err := dev.WriteAt(expected[i], 0); err != nil {
			t.Fatalf("seed data %d: %v", i+1, err)
		}
	}

	dataDevs := []BlockDevice{data[0], data[1], data[2]}
	parityDevs := []BlockDevice{parity[0], parity[1]}
	engine, err := NewEngine(dataDevs, []int64{size, size, size}, parityDevs, size)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := engine.BuildParity(64 << 10); err != nil {
		t.Fatalf("BuildParity: %v", err)
	}

	for _, missing := range [][]int{{0}, {1, 2}} {
		for _, slot := range missing {
			got, err := engine.ReconstructData(slot, missing, 0, int(size))
			if err != nil {
				t.Fatalf("ReconstructData slot %d missing %v: %v", slot+1, missing, err)
			}
			if !bytes.Equal(got, expected[slot]) {
				t.Fatalf("reconstructed slot %d with missing %v does not match", slot+1, missing)
			}
		}
	}
}
