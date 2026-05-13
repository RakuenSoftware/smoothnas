package fsarray

import (
	"reflect"
	"testing"
)

func TestBuildPlanBtrfsDefaults(t *testing.T) {
	plan, err := BuildPlan("fast", KindBtrfs, "", "", "", 0, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 200},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.DataProfile != "single" || plan.MetadataProfile != "raid1" {
		t.Fatalf("profiles = %s/%s, want single/raid1", plan.DataProfile, plan.MetadataProfile)
	}
	if plan.Label != "smoothnas-btrfs-fast" || plan.MountPath != "/mnt/fast" || plan.SizeBytes != 300 {
		t.Fatalf("unexpected plan: %#v", plan)
	}

	plan, err = BuildPlan("single", KindBtrfs, "", "", "", 0, []Device{
		{Path: "/dev/sdb", Size: 100},
	})
	if err != nil {
		t.Fatalf("single-disk BuildPlan: %v", err)
	}
	if plan.DataProfile != "single" || plan.MetadataProfile != "dup" {
		t.Fatalf("single-disk profiles = %s/%s, want single/dup", plan.DataProfile, plan.MetadataProfile)
	}
}

func TestBuildPlanBcachefsReplicas(t *testing.T) {
	if _, err := BuildPlan("archive", KindBcachefs, "", "", "", 3, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 100},
	}); err == nil {
		t.Fatal("expected replicas > device count to fail")
	}
	plan, err := BuildPlan("archive", KindBcachefs, "", "", "", 2, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 100},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.Replicas != 2 || plan.DataProfile != "" || plan.MetadataProfile != "" {
		t.Fatalf("unexpected bcachefs plan: %#v", plan)
	}
}

func TestBuildCommandArgs(t *testing.T) {
	plan, err := BuildPlan("fast", KindBtrfs, "", "raid1", "raid1", 0, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 200},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	wantBtrfs := []string{"mkfs.btrfs", "-f", "-L", "smoothnas-btrfs-fast", "-d", "raid1", "-m", "raid1", "/dev/sdb", "/dev/sdc"}
	if got := BuildMkfsBtrfsArgs(plan); !reflect.DeepEqual(got, wantBtrfs) {
		t.Fatalf("btrfs args = %#v, want %#v", got, wantBtrfs)
	}

	plan, err = BuildPlan("cache", KindBcachefs, "", "", "", 2, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 200},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	wantBcachefs := []string{"bcachefs", "format", "--replicas=2", "/dev/sdb", "/dev/sdc"}
	if got := BuildBcachefsFormatArgs(plan); !reflect.DeepEqual(got, wantBcachefs) {
		t.Fatalf("bcachefs args = %#v, want %#v", got, wantBcachefs)
	}
	if got := BcachefsMountSource(plan); got != "/dev/sdb:/dev/sdc" {
		t.Fatalf("mount source = %q", got)
	}
}
