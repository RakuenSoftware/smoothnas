package api

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/tier"
)

type smoothfsMetadataMaskRecommendation struct {
	Mask   uint64
	Reason string
	OK     bool
}

func (h *SmoothfsHandler) recommendMetadataActiveTierMask(pool db.SmoothfsPool) smoothfsMetadataMaskRecommendation {
	if len(pool.Tiers) == 0 {
		return smoothfsMetadataMaskRecommendation{Mask: 1, Reason: "pool has no recorded tiers", OK: false}
	}

	slots, err := h.store.ListTierSlots(pool.Name)
	if err != nil {
		return smoothfsMetadataMaskRecommendation{Mask: 1, Reason: "pool is not a managed SmoothNAS tier pool", OK: false}
	}

	slotsByPath := make(map[string]db.TierSlot, len(slots))
	for _, slot := range slots {
		if slot.State == db.TierSlotStateEmpty {
			continue
		}
		path := filepath.Clean(tier.PerTierBackingMount(pool.Name, slot.Name))
		slotsByPath[path] = slot
	}

	type indexedSlot struct {
		index int
		slot  db.TierSlot
	}
	var selected []indexedSlot
	mask := uint64(1)
	var reasons []string
	for idx, tierPath := range pool.Tiers {
		if idx == 0 {
			continue
		}
		if idx >= 64 {
			reasons = append(reasons, fmt.Sprintf("tier %d cannot fit in metadata-active mask", idx))
			continue
		}
		slot, ok := slotsByPath[filepath.Clean(tierPath)]
		if !ok {
			reasons = append(reasons, fmt.Sprintf("tier %d is not a managed SmoothNAS backing", idx))
			return smoothfsMetadataMaskRecommendation{Mask: mask, Reason: strings.Join(reasons, "; "), OK: false}
		}
		selected = append(selected, indexedSlot{index: idx, slot: slot})
	}
	if len(selected) == 0 {
		return smoothfsMetadataMaskRecommendation{Mask: mask, Reason: "only the fastest tier is recorded", OK: true}
	}

	mdadmMembers := map[string][]string{}
	if arrays, err := listMDADMArrays(); err == nil {
		for _, array := range arrays {
			mdadmMembers[array.Path] = append([]string(nil), array.MemberDisks...)
		}
	}

	disks, err := listDisksForSpindown()
	if err != nil {
		return smoothfsMetadataMaskRecommendation{Mask: 1, Reason: "could not list disks to compute metadata-active tier mask", OK: false}
	}
	rotational := make(map[string]bool, len(disks))
	for _, d := range disks {
		rotational[disk.BaseDiskPath(d.Path)] = d.Rotational
	}

	for _, item := range selected {
		active, reason := managedTierSlotExternallyActive(item.slot, mdadmMembers, rotational)
		if active {
			mask |= uint64(1) << item.index
			continue
		}
		reasons = append(reasons, fmt.Sprintf("tier %s inactive: %s", item.slot.Name, reason))
	}

	reason := "all managed HDD tiers are active or masked out"
	if len(reasons) > 0 {
		reason = strings.Join(reasons, "; ")
	}
	return smoothfsMetadataMaskRecommendation{Mask: mask, Reason: reason, OK: true}
}

func managedTierSlotExternallyActive(slot db.TierSlot, mdadmMembers map[string][]string, rotational map[string]bool) (bool, string) {
	devices := managedTierSlotDevices(slot, mdadmMembers)
	if len(devices) == 0 {
		return false, "no backing devices were found"
	}

	checkedRotational := false
	for _, device := range devices {
		base := disk.BaseDiskPath(device)
		isRotational, known := rotational[base]
		if !known {
			return false, "could not confirm backing disk type"
		}
		if !isRotational {
			continue
		}
		checkedRotational = true
		state, err := queryPowerStateForSpindown(base)
		if err != nil {
			return false, "could not confirm backing HDD power state"
		}
		diskPowerObserver.Observe(base, state, "external activity observed by smoothfs metadata gate")
		switch state {
		case "active", "idle":
			continue
		default:
			return false, "backing HDD is not confirmed active"
		}
	}
	if checkedRotational {
		return true, "all backing HDDs are active"
	}
	return true, "tier has no rotational backing disks"
}

func managedTierSlotDevices(slot db.TierSlot, mdadmMembers map[string][]string) []string {
	switch slot.BackingKind {
	case "zfs":
		return zfsMemberDevicesForSpindown(slot.BackingRef)
	case "", "mdadm":
		if slot.PVDevice == nil {
			return nil
		}
		if members := mdadmMembers[*slot.PVDevice]; len(members) > 0 {
			return append([]string(nil), members...)
		}
		return []string{*slot.PVDevice}
	default:
		if slot.BackingRef != "" {
			return []string{slot.BackingRef}
		}
	}
	return nil
}
