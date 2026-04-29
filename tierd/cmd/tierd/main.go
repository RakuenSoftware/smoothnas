package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	smoothfscontrol "github.com/RakuenSoftware/smoothfs/controlplane"
	sgauth "github.com/RakuenSoftware/smoothgui/auth"

	"github.com/JBailes/SmoothNAS/tierd/internal/api"
	"github.com/JBailes/SmoothNAS/tierd/internal/backup"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/monitor"
	"github.com/JBailes/SmoothNAS/tierd/internal/network"
	"github.com/JBailes/SmoothNAS/tierd/internal/nfs"
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

const tierdUsage = `tierd — SmoothNAS storage management daemon

Usage:
  tierd                    Run the daemon (typically invoked by systemd)
  tierd --version          Print version and exit
  tierd --help             Print this help and exit

The daemon listens on 127.0.0.1:8420. For operator commands use tierd-cli.
`

const defaultAddr = "127.0.0.1:8420"

// systemd-networkd config dir + sysfs root for the default-bond
// policy. Match the NetworkHandler defaults so all writes land in
// the same place.
const (
	defaultNetworkDir  = "/etc/systemd/network"
	defaultSysClassNet = "/sys/class/net"
)

// version is set at build time via -ldflags.
var version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__pam_auth":
			os.Exit(sgauth.RunPAMHelper(os.Args[2:]))
		case "__host_init":
			runHostInit()
			return
		case "--version", "-v", "version":
			fmt.Println(version)
			return
		case "--help", "-h", "help":
			fmt.Print(tierdUsage)
			return
		default:
			fmt.Fprintf(os.Stderr, "tierd: unknown argument %q\n\n", os.Args[1])
			fmt.Fprint(os.Stderr, tierdUsage)
			os.Exit(2)
		}
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

	// Any runs marked "running" from a previous tierd instance are now orphaned.
	if err := store.MarkStaleRunsFailed(); err != nil {
		log.Printf("warning: could not mark stale backup runs as failed: %v", err)
	}
	if err := api.ReconcileSharingConfig(store); err != nil {
		log.Printf("warning: could not reconcile sharing config: %v", err)
	}

	// First-boot default-bond policy: a fresh appliance gets bond0
	// over every physical Ethernet NIC in balance-alb mode, DHCPed.
	// Once the bootstrap marker is set, this is a no-op so an
	// operator's Break Bond / static-IP intent survives restarts.
	if err := network.ApplyDefaultBondPolicy(store, defaultNetworkDir, defaultSysClassNet); err != nil {
		log.Printf("warning: could not apply default bond policy: %v", err)
	}

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

	// Boot-time adapter reconciliation: ensures smoothfs-backed namespace
	// processes exist for all healthy pools. Runs in the background so it
	// does not delay HTTP readiness. After reconcile, open a per-pool meta
	// store on each pool's fastest-tier backing mount so the planner
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

	// Optional pprof endpoint for live profiling. Off by default; set
	// TIERD_PPROF_ADDR to bind (e.g. 127.0.0.1:6060). Intentionally
	// separate from the API listener so it is never exposed through
	// nginx and never shares auth/TLS surface with the control API.
	// net/http/pprof registers its handlers on http.DefaultServeMux as
	// a side-effect of import, so passing nil here wires them up.
	if pprofAddr := os.Getenv("TIERD_PPROF_ADDR"); pprofAddr != "" {
		go func() {
			log.Printf("pprof listening on %s (/debug/pprof/)", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				log.Printf("pprof listener exited: %v", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// smoothfs control plane (Phase 2). Treats "kernel module not loaded"
	// as feature-off and continues without it.
	smoothCtx, smoothCancel := context.WithCancel(context.Background())
	smoothSvc, smoothErr := smoothfscontrol.NewService(smoothCtx, store.DB(), 4)
	if smoothErr != nil {
		log.Printf("smoothfs: not enabled (%v)", smoothErr)
		smoothSvc = nil
	} else {
		go func() {
			if err := smoothSvc.Run(smoothCtx); err != nil {
				log.Printf("smoothfs: service ended: %v", err)
			}
		}()
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

	// Cancel the control plane before closing the netlink sockets so
	// runEvents exits on context cancellation instead of logging a shutdown
	// race from a closed descriptor.
	smoothCancel()
	if smoothSvc != nil {
		if err := smoothSvc.Close(); err != nil {
			log.Printf("smoothfs: close: %v", err)
		}
	}

	log.Println("stopped")
}

func runHostInit() {
	log.Printf("tierd %s host init starting", version)

	// Tear down /tmp/smoothnas-backup-* mounts left behind by a previous tierd
	// killed mid-backup. `defer umount` in rsyncMount/runCP does not run on
	// SIGKILL or crash.
	backup.CleanupOrphanedMounts()

	// Ensure the tierd system group exists before the API/auth layer comes up.
	users := sgauth.NewUserManager("tierd")
	if err := users.EnsureGroup(); err != nil {
		log.Printf("warning: could not ensure tierd group: %v", err)
	}

	// Host-level remediation and tuning are boot-time concerns, not part of the
	// long-lived control-plane process.
	updater.EnsureSystemPackages()
	if err := nfs.ApplyServerTuning(); err != nil {
		log.Printf("warning: could not apply NFS tuning: %v", err)
	}
	mdadm.EnsureStripeCacheSize(mdadm.DefaultStripeCachePages)
	tuning.ApplyNetworkTuning()
	tuning.ApplyBlockTuning()
	if err := nfs.ApplyServerTuning(); err != nil {
		log.Printf("warning: could not apply NFS tuning: %v", err)
	}

	log.Printf("tierd %s host init complete", version)
}

// openPoolMetaStores resolves each tier pool's fastest (lowest-rank) tier
// slot, waits for its backing mount to be ready, and opens a PoolMetaStore
// under `.tierd-meta/`. Registered with the mdadm adapter so the hot
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
		// fastest down to slower tiers under capacity pressure — unless
		// the pool was created with meta_on_fastest, in which case only
		// the fastest tier's backing holds metadata.
		var tierBackings []meta.TierBacking
		for i := range slots {
			if slots[i].State != "assigned" {
				continue
			}
			if p.MetaOnFastest && slots[i].Rank != fastest.Rank {
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
		// or placed outside of smoothfs). Idempotent and preserves pin state.
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
			// Then once an hour: catches files placed outside of smoothfs and
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
