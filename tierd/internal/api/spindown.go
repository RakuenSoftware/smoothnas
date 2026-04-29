package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/spindown"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
)

type spindownMaintenanceBlock struct {
	Pool         string `json:"pool"`
	NextActiveAt string `json:"next_active_at,omitempty"`
	Reason       string `json:"reason"`
}

var spindownNow = time.Now
var listDisksForSpindown = disk.List
var queryPowerStateForSpindown = disk.QueryPowerState
var zfsMemberDevicesForSpindown = zfs.MemberDevices

func poolMaintenanceDecision(store *db.Store, poolName string, now time.Time) (spindown.Decision, error) {
	devices, err := poolBackingDevices(store, poolName)
	if err != nil {
		return spindown.Decision{}, err
	}
	return maintenanceDecision(store, spindown.PoolEnabledKey(poolName), spindown.PoolWindowsKey(poolName), devices, now)
}

func zfsMaintenanceDecision(store *db.Store, poolName string, now time.Time) (spindown.Decision, error) {
	return maintenanceDecision(store, spindown.ZFSEnabledKey(poolName), spindown.ZFSWindowsKey(poolName), zfsMemberDevicesForSpindown(poolName), now)
}

func maintenanceDecision(store *db.Store, enabledKey, windowsKey string, devices []string, now time.Time) (spindown.Decision, error) {
	enabled, err := spindown.Enabled(store, enabledKey)
	if err != nil {
		return spindown.Decision{}, err
	}
	if !enabled {
		return spindown.Decision{Allowed: true}, nil
	}
	blocked, reason, err := standbyBlockForDevices(devices)
	if err != nil {
		return spindown.Decision{}, err
	}
	if blocked {
		_, windows, err := spindown.DecisionFor(store, enabledKey, windowsKey, now)
		if err != nil {
			return spindown.Decision{}, err
		}
		decision := spindown.Decision{
			Allowed: false,
			Reason:  reason,
		}
		if next, ok := spindown.NextActive(windows, now); ok {
			decision.NextActiveAt = next.UTC().Format(time.RFC3339)
		}
		return decision, nil
	}
	if len(devices) > 0 {
		return spindown.Decision{Allowed: true, ActiveNow: true}, nil
	}
	decision, _, err := spindown.DecisionFor(store, enabledKey, windowsKey, now)
	return decision, err
}

func standbyBlockForDevices(devices []string) (bool, string, error) {
	if len(devices) == 0 {
		return false, "", nil
	}
	disks, err := listDisksForSpindown()
	if err != nil {
		return false, "", err
	}
	rotational := make(map[string]bool, len(disks))
	for _, d := range disks {
		rotational[disk.BaseDiskPath(d.Path)] = d.Rotational
	}
	for _, device := range devices {
		base := disk.BaseDiskPath(device)
		isRotational, known := rotational[base]
		if !known {
			return true, "could not confirm backing disks are already active", nil
		}
		if !isRotational {
			continue
		}
		state, err := queryPowerStateForSpindown(base)
		if err != nil {
			return true, "could not confirm backing HDD is already active", nil
		}
		diskPowerObserver.Observe(base, state, "external activity observed by maintenance guard")
		if state == "standby" || state == "sleeping" {
			return true, "backing HDD is in standby; waiting for external activity", nil
		}
	}
	return false, "", nil
}

func rejectBlockedMaintenance(w http.ResponseWriter, poolName, action string, decision spindown.Decision) bool {
	if decision.Allowed {
		return false
	}
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":          action + " is deferred by the pool's spindown policy",
		"pool":           poolName,
		"reason":         decision.Reason,
		"next_active_at": decision.NextActiveAt,
	})
	return true
}

func spindownBlockedTierPools(store *db.Store, now time.Time) ([]spindownMaintenanceBlock, error) {
	pools, err := store.ListTierInstances()
	if err != nil {
		return nil, err
	}
	var blocked []spindownMaintenanceBlock
	for _, pool := range pools {
		decision, err := poolMaintenanceDecision(store, pool.Name, now)
		if err != nil {
			return nil, err
		}
		if decision.Allowed {
			continue
		}
		blocked = append(blocked, spindownMaintenanceBlock{
			Pool:         pool.Name,
			NextActiveAt: decision.NextActiveAt,
			Reason:       decision.Reason,
		})
	}
	return blocked, nil
}

func poolBackingDevices(store *db.Store, poolName string) ([]string, error) {
	slots, err := store.ListTierSlots(poolName)
	if err != nil {
		return nil, err
	}
	mdadmMembers := map[string][]string{}
	arrays, err := listMDADMArrays()
	if err == nil {
		for _, array := range arrays {
			mdadmMembers[array.Path] = append([]string(nil), array.MemberDisks...)
		}
	}
	devicesByPath := make(map[string]bool)
	var devices []string
	add := func(path string) {
		if path == "" || devicesByPath[path] {
			return
		}
		devicesByPath[path] = true
		devices = append(devices, path)
	}
	for _, slot := range slots {
		if slot.State == db.TierSlotStateEmpty {
			continue
		}
		switch slot.BackingKind {
		case "zfs":
			for _, dev := range zfsMemberDevicesForSpindown(slot.BackingRef) {
				add(dev)
			}
		default:
			if slot.PVDevice != nil {
				if members := mdadmMembers[*slot.PVDevice]; len(members) > 0 {
					for _, dev := range members {
						add(dev)
					}
				} else {
					add(*slot.PVDevice)
				}
			}
		}
	}
	return devices, nil
}

func zfsTierOwners(store *db.Store, zfsPool string) ([]string, error) {
	pools, err := store.ListTierInstances()
	if err != nil {
		return nil, err
	}
	var owners []string
	for _, pool := range pools {
		slots, err := store.ListTierSlots(pool.Name)
		if err != nil {
			return nil, err
		}
		for _, slot := range slots {
			if slot.BackingKind == "zfs" && slot.BackingRef == zfsPool && slot.State != db.TierSlotStateEmpty {
				owners = append(owners, pool.Name)
				break
			}
		}
	}
	return owners, nil
}
