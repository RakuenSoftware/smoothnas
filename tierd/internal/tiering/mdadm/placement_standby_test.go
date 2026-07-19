package mdadm

import (
	"errors"
	"testing"

	diskpkg "github.com/JBailes/SmoothNAS/tierd/internal/disk"
)

// TestBackingDevicesStandbyBlocked_FailOpen locks in the mover-eligibility
// guard's fail-open contract: it defers a tiering move ONLY when a backing HDD
// is positively confirmed standby/sleeping. Every "can't tell" condition — a
// failed disk listing, a backing device absent from the listing (e.g. the md
// array itself when member enumeration misses), or a drive that reports no ATA
// power state ("unknown") — must NOT defer. Deferring on unconfirmable state
// permanently deadlocks tiering on hardware that never reports a power state
// and strands in-flight moves (their smoothfs objects then resolve to ENOENT).
func TestBackingDevicesStandbyBlocked_FailOpen(t *testing.T) {
	origList := backingDiskList
	origQuery := backingQueryPowerState
	t.Cleanup(func() {
		backingDiskList = origList
		backingQueryPowerState = origQuery
	})

	hdd := diskpkg.Disk{Path: "/dev/sda", Rotational: true}
	stub := errors.New("stub")

	cases := []struct {
		name        string
		disks       []diskpkg.Disk
		listErr     error
		devices     []string
		powerState  string
		powerErr    error
		wantBlocked bool
	}{
		{"unknown power state proceeds", []diskpkg.Disk{hdd}, nil, []string{"/dev/sda"}, "unknown", nil, false},
		{"backing md array missing from listing proceeds", []diskpkg.Disk{hdd}, nil, []string{"/dev/md1"}, "", nil, false},
		{"power query error proceeds", []diskpkg.Disk{hdd}, nil, []string{"/dev/sda"}, "", stub, false},
		{"list error proceeds", nil, stub, []string{"/dev/sda"}, "", nil, false},
		{"confirmed standby defers", []diskpkg.Disk{hdd}, nil, []string{"/dev/sda"}, "standby", nil, true},
		{"confirmed sleeping defers", []diskpkg.Disk{hdd}, nil, []string{"/dev/sda"}, "sleeping", nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backingDiskList = func() ([]diskpkg.Disk, error) { return tc.disks, tc.listErr }
			backingQueryPowerState = func(string) (string, error) { return tc.powerState, tc.powerErr }
			blocked, _ := backingDevicesStandbyBlocked(tc.devices)
			if blocked != tc.wantBlocked {
				t.Fatalf("blocked=%v, want %v", blocked, tc.wantBlocked)
			}
		})
	}
}
