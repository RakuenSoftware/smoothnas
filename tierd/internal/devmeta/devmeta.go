// Package devmeta reads and writes SmoothNAS tier metadata stored as a JSON
// file on the filesystem of the fastest tier. This metadata is the source of
// truth for pool/tier topology — the SQLite DB is a reconstructable cache.
//
// The metadata file is written at the root of the tier's mounted filesystem:
//
//	/mnt/.tierd-backing/{pool}/{tierName}/.smoothnas-meta.json
//
// Only the fastest tier (rank 1) stores metadata. It contains the complete
// pool topology including references to all peer tiers, so the entire pool
// can be reconstructed from a single file.
package devmeta

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MetadataFilename is the well-known filename written at the root of a tier's
// mounted filesystem.
const MetadataFilename = ".smoothnas-meta.json"

// TierMeta is the JSON payload stored on the fastest tier's filesystem.
type TierMeta struct {
	Version     int        `json:"version"`
	UUID        string     `json:"uuid"`
	PoolName    string     `json:"pool_name"`
	TierName    string     `json:"tier_name"`
	TierRank    int        `json:"tier_rank"`
	BackendKind string     `json:"backend_kind"`
	ArrayPath   string     `json:"array_path,omitempty"`
	CreatedAt   string     `json:"created_at"`
	Peers       []PeerTier `json:"peers,omitempty"`
}

// PeerTier describes another tier in the same pool.
type PeerTier struct {
	TierName    string `json:"tier_name"`
	TierRank    int    `json:"tier_rank"`
	BackendKind string `json:"backend_kind"`
	ArrayPath   string `json:"array_path,omitempty"`
}

// WriteMetadata writes a TierMeta as .smoothnas-meta.json in mountPath.
func WriteMetadata(mountPath string, meta *TierMeta) error {
	payload, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	path := filepath.Join(mountPath, MetadataFilename)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// ReadMetadata reads a TierMeta from mountPath/.smoothnas-meta.json.
func ReadMetadata(mountPath string) (*TierMeta, error) {
	path := filepath.Join(mountPath, MetadataFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta TierMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return &meta, nil
}

// NewTierMeta creates a TierMeta with a fresh UUID and current timestamp.
func NewTierMeta(poolName, tierName string, tierRank int, backendKind, arrayPath string) *TierMeta {
	return &TierMeta{
		Version:     1,
		UUID:        newUUID(),
		PoolName:    poolName,
		TierName:    tierName,
		TierRank:    tierRank,
		BackendKind: backendKind,
		ArrayPath:   arrayPath,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

// ScanResult is a single metadata file discovered during a mount scan.
type ScanResult struct {
	MountPath string
	Meta      *TierMeta
}

// ScanMounts scans mounted filesystems for .smoothnas-meta.json files.
func ScanMounts() ([]ScanResult, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil, fmt.Errorf("read /proc/mounts: %w", err)
	}
	var results []ScanResult
	seen := map[string]bool{}
	for _, line := range splitLines(string(data)) {
		fields := splitFields(line)
		if len(fields) < 2 {
			continue
		}
		mp := fields[1]
		if seen[mp] {
			continue
		}
		seen[mp] = true
		meta, err := ReadMetadata(mp)
		if err != nil {
			continue
		}
		results = append(results, ScanResult{MountPath: mp, Meta: meta})
	}
	return results, nil
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func splitFields(s string) []string {
	var fields []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			if start >= 0 {
				fields = append(fields, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, s[start:])
	}
	return fields
}
