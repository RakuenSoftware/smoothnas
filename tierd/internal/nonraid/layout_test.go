package nonraid

import "testing"

func TestBuildPlanValidatesParityLimit(t *testing.T) {
	data := []Device{
		{Path: "/dev/sdb", Serial: "data1", Size: 8 << 40},
		{Path: "/dev/sdc", Serial: "data2", Size: 10 << 40},
	}
	parity := []Device{
		{Path: "/dev/sdd", Serial: "parity1", Size: 12 << 40},
		{Path: "/dev/sde", Serial: "parity2", Size: 10 << 40},
	}

	plan, err := BuildPlan("media", "", "", data, parity)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if plan.ParityCount != 2 {
		t.Fatalf("ParityCount = %d, want 2", plan.ParityCount)
	}
	if plan.MinParityBytes != 10<<40 {
		t.Fatalf("MinParityBytes = %d, want %d", plan.MinParityBytes, uint64(10<<40))
	}
	if plan.CapacityBytes != 18<<40 {
		t.Fatalf("CapacityBytes = %d, want %d", plan.CapacityBytes, uint64(18<<40))
	}
	if len(plan.Devices) != 4 {
		t.Fatalf("devices = %d, want 4", len(plan.Devices))
	}
}

func TestBuildPlanRejectsDataLargerThanSmallestParity(t *testing.T) {
	_, err := BuildPlan("media", "", "",
		[]Device{{Path: "/dev/sdb", Size: 12 << 40}},
		[]Device{{Path: "/dev/sdc", Size: 10 << 40}},
	)
	if err == nil {
		t.Fatal("BuildPlan succeeded with oversized data disk")
	}
}

func TestBuildPlanRejectsDuplicateDevices(t *testing.T) {
	_, err := BuildPlan("media", "", "",
		[]Device{{Path: "/dev/sdb", Size: 8 << 40}},
		[]Device{{Path: "/dev/sdb", Size: 10 << 40}},
	)
	if err == nil {
		t.Fatal("BuildPlan succeeded with duplicate disk")
	}
}

func TestBuildPlanRequiresOneToThreeParityDisks(t *testing.T) {
	data := []Device{{Path: "/dev/sdb", Size: 8 << 40}}
	for _, parity := range [][]Device{
		nil,
		{
			{Path: "/dev/sdc", Size: 10 << 40},
			{Path: "/dev/sdd", Size: 10 << 40},
			{Path: "/dev/sde", Size: 10 << 40},
			{Path: "/dev/sdf", Size: 10 << 40},
		},
	} {
		if _, err := BuildPlan("media", "", "", data, parity); err == nil {
			t.Fatalf("BuildPlan succeeded with %d parity disks", len(parity))
		}
	}
}
