package tiermeta

import "fmt"

// lvmExtentSize is the default LVM physical extent size (4 MiB).
// All metadata LV sizes are rounded up to this boundary.
const lvmExtentSize = 4 * 1024 * 1024

// MetaLVSizeBytes returns the metadata LV size for a single tier PV:
// 0.1% of pvBytes, rounded up to the nearest LVM extent, with a minimum
// of one extent (4 MiB).
func MetaLVSizeBytes(pvBytes uint64) uint64 {
	if pvBytes == 0 {
		return lvmExtentSize
	}
	tenth := pvBytes / 1000 // 0.1 %
	if tenth == 0 {
		return lvmExtentSize
	}
	// Round up to nearest extent boundary.
	return ((tenth + lvmExtentSize - 1) / lvmExtentSize) * lvmExtentSize
}

// CompleteMetaLVSizeBytes returns the size for the complete-metadata LV on
// the slowest tier: 0.1% of the combined size of all tier PVs.
func CompleteMetaLVSizeBytes(pvSizes []uint64) uint64 {
	var total uint64
	for _, s := range pvSizes {
		total += s
	}
	return MetaLVSizeBytes(total)
}

// BytesToLVMSize formats n as a byte-count string accepted by lvcreate -L.
func BytesToLVMSize(n uint64) string {
	return fmt.Sprintf("%dB", n)
}
