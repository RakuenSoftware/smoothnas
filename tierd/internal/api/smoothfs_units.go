package api

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tier"
	smoothfsclient "github.com/RakuenSoftware/smoothfs"
	"github.com/google/uuid"
)

var runSystemctl = func(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

var createManagedPoolForSystem = createManagedPool
var destroyManagedPoolForSystem = destroyManagedPool
var managedSmoothfsTierUsable = isPathMounted

const defaultSmoothfsAdmissionFullPct = 95

func renderManagedPoolUnit(p smoothfsclient.ManagedPool) (string, error) {
	return smoothfsclient.RenderMountUnit(p)
}

func createManagedPool(req smoothfsclient.CreateManagedPoolRequest) (*smoothfsclient.ManagedPool, error) {
	if err := smoothfsclient.ValidatePoolName(req.Name); err != nil {
		return nil, err
	}
	if err := smoothfsclient.ValidateTiers(req.Tiers); err != nil {
		return nil, err
	}

	poolUUID := req.UUID
	if poolUUID == uuid.Nil {
		poolUUID = uuid.New()
	}
	base := req.MountBase
	if base == "" {
		base = smoothfsclient.DefaultMountBase
	}
	mountpoint := smoothfsclient.MountpointForPool(base, req.Name)
	pool := smoothfsclient.ManagedPool{
		Name:       req.Name,
		UUID:       poolUUID,
		Tiers:      req.Tiers,
		Mountpoint: mountpoint,
		UnitPath:   filepath.Join(smoothfsclient.SystemdUnitDir, smoothfsclient.UnitFilenameFor(mountpoint)),
	}
	if err := os.MkdirAll(pool.Mountpoint, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir mountpoint %s: %w", pool.Mountpoint, err)
	}
	body, err := renderManagedPoolUnit(pool)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(pool.UnitPath, []byte(body), 0o644); err != nil {
		return nil, fmt.Errorf("write unit %s: %w", pool.UnitPath, err)
	}
	rollback := func() { _ = os.Remove(pool.UnitPath) }

	if err := runSystemctl("daemon-reload"); err != nil {
		rollback()
		return nil, fmt.Errorf("daemon-reload: %w", err)
	}
	if err := runSystemctl("enable", "--now", filepath.Base(pool.UnitPath)); err != nil {
		rollback()
		_ = runSystemctl("daemon-reload")
		return nil, fmt.Errorf("enable --now %s: %w", pool.UnitPath, err)
	}
	return &pool, nil
}

func destroyManagedPool(p smoothfsclient.ManagedPool) error {
	unitName := filepath.Base(p.UnitPath)
	var firstErr error
	setErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	if err := runSystemctl("disable", "--now", unitName); err != nil {
		setErr(fmt.Errorf("disable --now: %w", err))
	}
	if err := os.Remove(p.UnitPath); err != nil && !os.IsNotExist(err) {
		setErr(fmt.Errorf("remove unit: %w", err))
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		setErr(fmt.Errorf("daemon-reload: %w", err))
	}
	return firstErr
}

func rewriteManagedPoolUnit(pool db.SmoothfsPool) error {
	if pool.UnitPath == "" {
		return nil
	}
	parsed, err := uuid.Parse(pool.UUID)
	if err != nil {
		return err
	}
	mp := smoothfsclient.ManagedPool{
		Name:       pool.Name,
		UUID:       parsed,
		Tiers:      pool.Tiers,
		Mountpoint: pool.Mountpoint,
		UnitPath:   pool.UnitPath,
	}
	if err := os.MkdirAll(mp.Mountpoint, 0o755); err != nil {
		return fmt.Errorf("mkdir mountpoint %s: %w", mp.Mountpoint, err)
	}
	body, err := renderManagedPoolUnit(mp)
	if err != nil {
		return err
	}
	if err := os.WriteFile(mp.UnitPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write unit %s: %w", mp.UnitPath, err)
	}
	return runSystemctl("daemon-reload")
}

func managedSmoothfsAdmissionFullThreshold(store *db.Store, poolName string) (int, error) {
	slots, err := store.ListTierSlots(poolName)
	if err != nil {
		return 0, err
	}

	slowestRank := 0
	for _, slot := range slots {
		if slot.State != db.TierSlotStateAssigned {
			continue
		}
		if slot.Rank > slowestRank {
			slowestRank = slot.Rank
		}
	}

	threshold := 0
	for _, slot := range slots {
		if slot.State != db.TierSlotStateAssigned || slot.Rank == slowestRank {
			continue
		}
		if slot.FullThresholdPct <= 0 {
			continue
		}
		if threshold == 0 || slot.FullThresholdPct < threshold {
			threshold = slot.FullThresholdPct
		}
	}
	if threshold == 0 {
		threshold = defaultSmoothfsAdmissionFullPct
	}
	if threshold < 1 {
		return 1, nil
	}
	if threshold > 100 {
		return 100, nil
	}
	return threshold, nil
}

func syncManagedSmoothfsAdmissionThreshold(store *db.Store, poolName string) error {
	pool, err := store.GetSmoothfsPool(poolName)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return err
	}
	threshold, err := managedSmoothfsAdmissionFullThreshold(store, poolName)
	if err != nil {
		return err
	}
	root := smoothfsWriteStagingRoot(pool.UUID)
	if !sysfsBool(filepath.Join(root, "write_staging_supported")) {
		return nil
	}
	path := filepath.Join(root, "write_staging_full_pct")
	if sysfsUint(path) == uint64(threshold) {
		return nil
	}
	data := []byte(strconv.Itoa(threshold) + "\n")
	return writeSmoothfsWriteStagingFile(path, data, 0o644)
}

func isPathMounted(path string) bool {
	return exec.Command("findmnt", "-M", path).Run() == nil
}

func ensureManagedSmoothfsPool(store *db.Store, poolName string) error {
	tiers, err := activeManagedSmoothfsTiers(store, poolName)
	if err != nil {
		return err
	}
	if len(tiers) == 0 {
		return fmt.Errorf("smoothfs pool %s has no mounted tier backings", poolName)
	}

	if existing, err := store.GetSmoothfsPool(poolName); err == nil {
		if existing.UnitPath != "" {
			if !sameStringSlice(existing.Tiers, tiers) || filepath.Clean(existing.Mountpoint) != filepath.Join("/mnt", poolName) {
				if err := replaceManagedSmoothfsPool(store, *existing, tiers); err != nil {
					return err
				}
				return nil
			}
			existing.Tiers = tiers
			if err := rewriteManagedPoolUnit(*existing); err != nil {
				return fmt.Errorf("rewrite existing smoothfs pool %s: %w", poolName, err)
			}
			if err := runSystemctl("enable", "--now", filepath.Base(existing.UnitPath)); err != nil {
				return fmt.Errorf("start existing smoothfs pool %s: %w", poolName, err)
			}
			if err := syncManagedSmoothfsAdmissionThreshold(store, poolName); err != nil {
				return fmt.Errorf("sync smoothfs admission threshold for %s: %w", poolName, err)
			}
		}
		return nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return err
	}

	return createManagedSmoothfsPoolRow(store, poolName, uuid.Nil, tiers)
}

func resumeManagedSmoothfsPools(store *db.Store, adapter interface{ Reconcile() error }) {
	if err := adapter.Reconcile(); err != nil {
		log.Printf("smoothfs mount resume: reconcile: %v", err)
	}
	pools, err := store.ListTierInstances()
	if err != nil {
		log.Printf("smoothfs mount resume: list tier pools: %v", err)
		return
	}
	for _, pool := range pools {
		if pool.State == db.TierPoolStateDestroying {
			continue
		}
		if err := ensureManagedSmoothfsPool(store, pool.Name); err != nil {
			log.Printf("smoothfs mount resume: ensure %s: %v", pool.Name, err)
		}
	}
}

func activeManagedSmoothfsTiers(store *db.Store, poolName string) ([]string, error) {
	slots, err := store.ListTierSlots(poolName)
	if err != nil {
		return nil, err
	}
	tiers := make([]string, 0, len(slots))
	for _, slot := range slots {
		if slot.State == db.TierSlotStateEmpty {
			continue
		}
		path := tier.PerTierBackingMount(poolName, slot.Name)
		if !managedSmoothfsTierUsable(path) {
			return nil, fmt.Errorf("tier backing %s/%s is assigned but %s is not mounted", poolName, slot.Name, path)
		}
		tiers = append(tiers, path)
	}
	return tiers, nil
}

func replaceManagedSmoothfsPool(store *db.Store, existing db.SmoothfsPool, tiers []string) error {
	parsed, err := uuid.Parse(existing.UUID)
	if err != nil {
		return err
	}
	mp := smoothfsclient.ManagedPool{
		Name:       existing.Name,
		UUID:       parsed,
		Tiers:      existing.Tiers,
		Mountpoint: existing.Mountpoint,
		UnitPath:   existing.UnitPath,
	}
	if err := destroyManagedPoolForSystem(mp); err != nil {
		return fmt.Errorf("replace smoothfs pool %s: destroy old mount: %w", existing.Name, err)
	}
	if err := store.DeleteSmoothfsPool(existing.Name); err != nil && !errors.Is(err, db.ErrNotFound) {
		return err
	}
	return createManagedSmoothfsPoolRow(store, existing.Name, parsed, tiers)
}

func createManagedSmoothfsPoolRow(store *db.Store, poolName string, poolUUID uuid.UUID, tiers []string) error {
	mp, err := createManagedPoolForSystem(smoothfsclient.CreateManagedPoolRequest{
		Name:      poolName,
		UUID:      poolUUID,
		Tiers:     tiers,
		MountBase: "/mnt",
	})
	if err != nil {
		return err
	}

	row := db.SmoothfsPool{
		UUID:       mp.UUID.String(),
		Name:       mp.Name,
		Tiers:      mp.Tiers,
		Mountpoint: mp.Mountpoint,
		UnitPath:   mp.UnitPath,
	}
	if _, err := store.CreateSmoothfsPool(row); err != nil {
		if errors.Is(err, db.ErrDuplicate) {
			return syncManagedSmoothfsAdmissionThreshold(store, poolName)
		}
		_ = destroyManagedPoolForSystem(*mp)
		return err
	}
	if err := syncManagedSmoothfsAdmissionThreshold(store, poolName); err != nil {
		return fmt.Errorf("sync smoothfs admission threshold for %s: %w", poolName, err)
	}
	return nil
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func destroyManagedSmoothfsPool(store *db.Store, poolName string) error {
	pool, err := store.GetSmoothfsPool(poolName)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return err
	}
	parsed, err := uuid.Parse(pool.UUID)
	if err != nil {
		return err
	}
	mp := smoothfsclient.ManagedPool{
		Name:       pool.Name,
		UUID:       parsed,
		Tiers:      pool.Tiers,
		Mountpoint: pool.Mountpoint,
		UnitPath:   pool.UnitPath,
	}
	destroyErr := destroyManagedPoolForSystem(mp)
	deleteErr := store.DeleteSmoothfsPool(poolName)
	if destroyErr != nil {
		return destroyErr
	}
	return deleteErr
}
