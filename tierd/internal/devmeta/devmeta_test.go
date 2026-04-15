package devmeta

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndReadMetadata(t *testing.T) {
	dir := t.TempDir()
	meta := NewTierMeta("media", "NVME", 1, "mdadm", "/dev/md1")
	meta.Peers = []PeerTier{
		{TierName: "HDD", TierRank: 3, BackendKind: "mdadm", ArrayPath: "/dev/md0"},
	}

	if err := WriteMetadata(dir, meta); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	// File should exist.
	if _, err := os.Stat(filepath.Join(dir, MetadataFilename)); err != nil {
		t.Fatalf("metadata file not found: %v", err)
	}

	got, err := ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	if got.PoolName != "media" {
		t.Errorf("PoolName = %q, want media", got.PoolName)
	}
	if got.TierName != "NVME" {
		t.Errorf("TierName = %q, want NVME", got.TierName)
	}
	if got.TierRank != 1 {
		t.Errorf("TierRank = %d, want 1", got.TierRank)
	}
	if got.UUID == "" {
		t.Error("UUID is empty")
	}
	if len(got.Peers) != 1 {
		t.Fatalf("Peers count = %d, want 1", len(got.Peers))
	}
	if got.Peers[0].TierName != "HDD" {
		t.Errorf("Peer TierName = %q, want HDD", got.Peers[0].TierName)
	}
}

func TestReadMetadataMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadMetadata(dir)
	if err == nil {
		t.Fatal("expected error for missing metadata, got nil")
	}
}

func TestWriteMetadataAtomic(t *testing.T) {
	dir := t.TempDir()
	meta := NewTierMeta("pool", "SSD", 2, "mdadm", "")

	if err := WriteMetadata(dir, meta); err != nil {
		t.Fatal(err)
	}

	// No .tmp file should remain.
	if _, err := os.Stat(filepath.Join(dir, MetadataFilename+".tmp")); !os.IsNotExist(err) {
		t.Error("temporary file was not cleaned up")
	}
}
