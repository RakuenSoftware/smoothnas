package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	sgauth "github.com/RakuenSoftware/smoothgui/auth"

	"github.com/JBailes/SmoothNAS/tierd/internal/api"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/monitor"
	"github.com/JBailes/SmoothNAS/tierd/internal/smart"
	"github.com/JBailes/SmoothNAS/tierd/internal/tier"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	mdadmadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
	zfsmgdadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/zfsmgd"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiermeta"
	"github.com/JBailes/SmoothNAS/tierd/internal/tuning"
	"github.com/JBailes/SmoothNAS/tierd/internal/updater"
)

const defaultAddr = "127.0.0.1:8420"

// version is set at build time via -ldflags.
var version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__pam_auth" {
		os.Exit(sgauth.RunPAMHelper(os.Args[2:]))
	}

	addr := os.Getenv("TIERD_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	dbPath := os.Getenv("TIERD_DB")
	if dbPath == "" {
		dbPath = "/var/lib/tierd/tierd.db"
	}

	store, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(); err != nil {
		log.Fatalf("failed to run migrations: %v", err)
	}

	// Migrate sharing tables.
	if err := store.MigrateShares(); err != nil {
		log.Fatalf("failed to migrate sharing tables: %v", err)
	}

	// Migrate mdadm tier state into the unified control-plane schema.
	if err := mdadmadapter.Migrate(store); err != nil {
		log.Fatalf("failed to migrate mdadm tier state: %v", err)
	}

	// Migrate backup tables.
	if err := store.MigrateBackups(); err != nil {
		log.Fatalf("failed to migrate backup tables: %v", err)
	}

	// Migrate backup run tracking table.
	if err := store.MigrateBackupRuns(); err != nil {
		log.Fatalf("failed to migrate backup_runs table: %v", err)
	}

	// Any runs marked "running" from a previous tierd instance are now orphaned.
	if err := store.MarkStaleRunsFailed(); err != nil {
		log.Printf("warning: could not mark stale backup runs as failed: %v", err)
	}

	// Ensure the tierd system group exists.
	users := sgauth.NewUserManager("tierd")
	if err := users.EnsureGroup(); err != nil {
		log.Printf("warning: could not ensure tierd group: %v", err)
	}

	// Ensure required OS packages are present. This covers existing installs
	// that predate a package being added to the required list, without needing
	// the user to apply a full update first. Runs in the background so it does
	// not delay startup.
	go updater.EnsureSystemPackages()

	// Heal stripe_cache_size on existing parity RAID arrays. The kernel
	// default of 256 pages cripples small random write performance on
	// RAID4/5/6.
	go mdadm.EnsureStripeCacheSize(mdadm.DefaultStripeCachePages)

	// Raise kernel networking and VM parameters for NAS throughput. Only
	// writes values that are below the targets, so operator overrides above
	// our defaults are preserved.
	go tuning.ApplyNetworkTuning()

	// Raise block device read-ahead for md arrays and member drives from the
	// kernel default of 128 KB to 4 MB. Only raises — never lowers.
	go tuning.ApplyBlockTuning()

	// Initialize SMART subsystem.
	historyStore, err := smart.NewHistoryStore(store.DB())
	if err != nil {
		log.Fatalf("failed to initialize SMART history: %v", err)
	}

	alarmStore, err := smart.NewAlarmStore(store.DB())
	if err != nil {
		log.Fatalf("failed to initialize SMART alarms: %v", err)
	}

	// Boot-time reconciliation: remount tier LVs, detect missing PVs, and
	// verify segment order.
	tierManager := tier.NewManager(store)

	// Initialise the per-tier LV metadata store.  Bootstrap() reads existing
	// tiermeta LVs from each tier's VG and populates the in-memory cache.
	// If the SQLite DB were lost, Bootstrap can reconstruct tier state from LVM.
	metaStore := tiermeta.NewStore()
	if err := metaStore.Bootstrap(); err != nil {
		log.Printf("tiermeta bootstrap: %v", err)
	}
	tierManager.SetMetaStore(metaStore)

	// Start background monitor.
	mon := monitor.New(historyStore, alarmStore)
	mon.SetArraySizeChangedCallback(func(arrayPath string) {
		tierManager.ExpandStorageForArray(arrayPath)
	})
	mon.Start()

	go tierManager.Reconcile()

	mdadmRunDir := os.Getenv("TIERD_MDADM_RUN_DIR")
	if mdadmRunDir == "" {
		mdadmRunDir = "/run/tierd/mdadm"
	}

	zfsRunDir := os.Getenv("TIERD_ZFS_RUN_DIR")
	if zfsRunDir == "" {
		zfsRunDir = "/run/tierd"
	}

	mdadmAdapter := mdadmadapter.NewAdapter(store, mdadmRunDir)
	zfsAdapter := zfsmgdadapter.NewAdapter(store, zfsRunDir)

	startTime := time.Now()
	router := api.NewRouterFull(store, version, startTime, historyStore, alarmStore, mon,
		mdadmAdapter,
		zfsAdapter,
	)

	// Boot-time adapter reconciliation: ensures FUSE namespaces and daemon
	// processes exist for all healthy pools. Runs in the background so it
	// does not delay HTTP readiness. After reconcile, open a per-pool meta
	// store on each pool's fastest-tier backing mount so the FUSE handlers
	// can record object placement without synchronous SQLite writes.
	go func() {
		if err := mdadmAdapter.Reconcile(); err != nil {
			log.Printf("mdadm adapter boot reconcile: %v", err)
		}
		if err := openPoolMetaStores(store, mdadmAdapter); err != nil {
			log.Printf("open pool meta stores: %v", err)
		}
		if err := openZFSPoolMetaStores(store, zfsAdapter); err != nil {
			log.Printf("open zfs pool meta stores: %v", err)
		}
		// Placement planner honours pin state: moves PinHot files onto the
		// fastest tier and PinCold files onto the slowest. Heat-driven
		// placement is not yet wired.
		mdadmAdapter.StartPlacementPlanner(context.Background())
	}()

	// Start per-adapter schedulers. Each runs a background planner cycle that
	// collects activity, plans and starts movements, and purges stale records.
	schedulers := []*tiering.Scheduler{
		tiering.NewScheduler(mdadmAdapter, store),
		tiering.NewScheduler(zfsAdapter, store),
	}
	for _, s := range schedulers {
		s.Start()
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("tierd %s listening on %s", version, addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-done
	log.Println("shutting down...")

	// Stop schedulers before shutting down the HTTP server so in-flight planner
	// cycles complete cleanly before the adapters are torn down.
	for _, s := range schedulers {
		s.Stop()
	}

	mon.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}

	// Flush every open meta store so we don't lose the last batch of
	// placement records on shutdown.
	mdadmAdapter.CloseMetaStores()
	zfsAdapter.CloseMetaStores()

	log.Println("stopped")
}

// openPoolMetaStores resolves each tier pool's fastest (lowest-rank) tier
// slot, waits for its backing mount to be ready, and opens a PoolMetaStore
// under `.tierd-meta/`. Registered with the mdadm adapter so the FUSE hot
// path can enqueue placement records asynchronously.
func openPoolMetaStores(store *db.Store, adapter *mdadmadapter.Adapter) error {
	pools, err := store.ListTierInstances()
	if err != nil {
		return err
	}
	for _, p := range pools {
		if p.State != db.TierPoolStateHealthy {
			continue
		}
		slots, err := store.ListTierSlots(p.Name)
		if err != nil {
			log.Printf("meta: list slots for pool %s: %v", p.Name, err)
			continue
		}
		var fastest *db.TierSlot
		for i := range slots {
			if slots[i].State != "assigned" {
				continue
			}
			if fastest == nil || slots[i].Rank < fastest.Rank {
				fastest = &slots[i]
			}
		}
		if fastest == nil {
			log.Printf("meta: pool %s has no assigned tier; skipping", p.Name)
			continue
		}
		// Build the per-tier list for the meta store. Every assigned and
		// mounted tier participates so cold metadata can spill from
		// fastest down to slower tiers under capacity pressure.
		var tierBackings []meta.TierBacking
		for i := range slots {
			if slots[i].State != "assigned" {
				continue
			}
			bm := tier.PerTierBackingMount(p.Name, slots[i].Name)
			if !lvm.IsMounted(bm) {
				continue
			}
			tierBackings = append(tierBackings, meta.TierBacking{
				Rank:         slots[i].Rank,
				Name:         slots[i].Name,
				BackingMount: bm,
			})
		}
		if len(tierBackings) == 0 {
			log.Printf("meta: pool %s has no mounted tier backings; skipping", p.Name)
			continue
		}
		ms, err := meta.Open(tierBackings)
		if err != nil {
			log.Printf("meta: open pool %s: %v", p.Name, err)
			continue
		}
		adapter.SetMetaStore(p.Name, ms)
		log.Printf("meta: opened tiered store for pool %s across %d tier(s) — fastest=%s",
			p.Name, len(tierBackings), fastest.Name)

		// Background reconcile: walk every tier's backing, prime the store
		// with records for files that pre-exist it (from before this change,
		// or placed outside of FUSE). Idempotent and preserves pin state.
		nsID, err := namespaceIDForPool(store, p.Name)
		if err != nil || nsID == "" {
			continue
		}
		sources := make([]meta.ReconcileSource, 0, len(slots))
		for i := range slots {
			if slots[i].State != "assigned" {
				continue
			}
			bm := tier.PerTierBackingMount(p.Name, slots[i].Name)
			if !lvm.IsMounted(bm) {
				continue
			}
			sources = append(sources, meta.ReconcileSource{
				BackingMount: bm,
				TierRank:     slots[i].Rank,
			})
		}
		if len(sources) == 0 {
			continue
		}
		go func(store *meta.PoolMetaStore, namespace string, srcs []meta.ReconcileSource) {
			// First reconcile right at startup.
			store.Reconcile(context.Background(), namespace, srcs)
			// Then once an hour: catches files placed outside of FUSE and
			// sweeps ghost records left behind by dropped delete enqueues.
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for range t.C {
				store.Reconcile(context.Background(), namespace, srcs)
			}
		}(ms, nsID, sources)
	}
	return nil
}

// openZFSPoolMetaStores opens per-pool meta stores for every ZFS-managed
// namespace. The store lives under the namespace's MountPath, inside a
// hidden .tierd-meta/ subdirectory — the same shape used by mdadm. No-op
// if no ZFS-managed namespaces exist.
func openZFSPoolMetaStores(store *db.Store, adapter *zfsmgdadapter.Adapter) error {
	nss, err := store.ListZFSManagedNamespaces()
	if err != nil {
		return err
	}
	for _, ns := range nss {
		if ns.MountPath == "" {
			continue
		}
		if _, err := os.Stat(ns.MountPath); err != nil {
			log.Printf("zfs meta: mount %s not ready: %v", ns.MountPath, err)
			continue
		}
		// ZFS-managed namespaces don't expose per-dataset rank info today,
		// so the meta store is single-tier (just MountPath itself).
		ms, err := meta.Open([]meta.TierBacking{{
			Rank:         1,
			Name:         "default",
			BackingMount: ns.MountPath,
		}})
		if err != nil {
			log.Printf("zfs meta: open %s: %v", ns.MountPath, err)
			continue
		}
		adapter.SetMetaStore(ns.PoolName, ms)
		log.Printf("zfs meta: opened store for pool %s at %s/.tierd-meta", ns.PoolName, ns.MountPath)
	}
	return nil
}

// namespaceIDForPool returns the mdadm-managed namespace ID for a pool,
// or empty string if none is registered yet.
func namespaceIDForPool(store *db.Store, poolName string) (string, error) {
	nss, err := store.ListMdadmManagedNamespaces()
	if err != nil {
		return "", err
	}
	for _, n := range nss {
		if n.PoolName == poolName {
			return n.NamespaceID, nil
		}
	}
	return "", nil
}

