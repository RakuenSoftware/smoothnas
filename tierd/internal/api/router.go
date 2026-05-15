package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	sgauth "github.com/RakuenSoftware/smoothgui/auth"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/health"
	"github.com/JBailes/SmoothNAS/tierd/internal/monitor"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin"
	"github.com/JBailes/SmoothNAS/tierd/internal/smart"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	"github.com/JBailes/SmoothNAS/tierd/internal/updater"
)

// NewRouter builds the HTTP handler tree for the tierd API.
func NewRouter(store *db.Store, version string, startTime time.Time) http.Handler {
	return NewRouterFull(store, version, startTime, nil, nil, nil)
}

// NewRouterFull builds the HTTP handler tree with all dependencies.
// adapters are registered with the tiering handler before the first request.
func NewRouterFull(store *db.Store, version string, startTime time.Time, historyStore *smart.HistoryStore, alarmStore *smart.AlarmStore, mon *monitor.Monitor, adapters ...tiering.TieringAdapter) http.Handler {
	return newRouterFull(store, version, startTime, historyStore, alarmStore, mon, nil, nil, adapters...)
}

// NewRouterFullWithPlugins builds the HTTP handler tree with the
// production plugin runtime lifecycle wired in. Tests and runtimeless
// developer environments can keep using NewRouterFull, which leaves
// lifecycle verbs returning 503.
func NewRouterFullWithPlugins(store *db.Store, version string, startTime time.Time, historyStore *smart.HistoryStore, alarmStore *smart.AlarmStore, mon *monitor.Monitor, pluginLifecycle *plugin.Lifecycle, pluginCatalog *plugin.Catalog, adapters ...tiering.TieringAdapter) http.Handler {
	return newRouterFull(store, version, startTime, historyStore, alarmStore, mon, pluginLifecycle, pluginCatalog, adapters...)
}

func newRouterFull(store *db.Store, version string, startTime time.Time, historyStore *smart.HistoryStore, alarmStore *smart.AlarmStore, mon *monitor.Monitor, pluginLifecycle *plugin.Lifecycle, pluginCatalog *plugin.Catalog, adapters ...tiering.TieringAdapter) http.Handler {
	healthHandler := health.NewHandler(version, startTime, health.RuntimeChecks(store))
	sessions := sgauth.NewSessionStore(store.DB(), 24*time.Hour)
	rateLimiter := sgauth.NewRateLimiter(store.DB(), 5, 15*time.Minute)
	users := sgauth.NewUserManager("tierd")
	authHandler := sgauth.NewHandler("tierd", sessions, rateLimiter, users)
	disksHandler := NewDisksHandler(store, historyStore, alarmStore)
	arraysHandler := NewArraysHandler(store)
	arraysHandler.ResumeDestroyingPools()
	// Plugin tier consumers — phase 03 of the plugin proposal.
	// Tier-deletion preflight refuses to destroy a pool that has
	// any plugin volumes pointing at it.
	pluginStore := plugin.NewStore(store)
	arraysHandler.SetPluginTierConsumers(pluginStore.TierConsumers)

	// Plugin profile catalog — phase 05.
	if pluginCatalog == nil {
		var catalogErr error
		pluginCatalog, catalogErr = plugin.NewCatalog(plugin.DefaultOperatorProfilesDir)
		if catalogErr != nil {
			log.Printf("plugin profile catalog: %v (built-ins still loaded)", catalogErr)
		}
	}
	pluginProfilesHandler := NewProfileHandler(pluginCatalog)

	// Phase 06a: HTTP plugin handler. Shares the same plugin.Store
	// + Installer + Lifecycle the tier-deletion blocker uses, so
	// every plugin-aware code path sees one consistent view of state.
	//
	// Production passes a Lifecycle when smoothnas-runtime is ready.
	// Runtimeless callers pass nil, and the HTTP layer surfaces that
	// as 503 on lifecycle verbs while install/list still work.
	pluginInstaller := plugin.NewInstaller(pluginStore)
	pluginInstaller.SetTierProvider(store, plugin.StatAvail)
	if pluginLifecycle != nil {
		pluginInstaller.SetDemolisher(pluginLifecycle)
	}
	pluginsHandler := NewPluginsHandler(pluginStore, pluginInstaller, pluginLifecycle, pluginCatalog, store)
	zfsHandler := NewZFSHandler(store)
	userPrefsHandler := NewUserPrefsHandler(store)
	sharingHandler := NewSharingHandler(store)
	networkHandler := NewNetworkHandler(store)
	benchmarkHandler := NewBenchmarkHandler()
	networkTestsHandler := NewNetworkTestsHandler()
	upd := updater.New(version)
	systemHandler := NewSystemHandler(mon, upd)
	jobsHandler := NewJobsHandler()
	tieringHandler := NewTieringHandler(store)
	smoothfsHandler := NewSmoothfsHandler(store)
	for _, a := range adapters {
		if err := tieringHandler.RegisterAdapter(a); err != nil {
			log.Printf("tiering: skipping adapter %q: %v", a.Kind(), err)
		}
	}
	// Wire automatic namespace creation: after per-tier provisioning
	// succeeds the handler triggers adapter reconciliation which ensures
	// tier targets, managed targets, namespace, and smoothfs kernel module all exist.
	for _, a := range adapters {
		if a.Kind() == "mdadm" {
			adapter := a
			arraysHandler.SetEnsureNamespace(func(poolName string) error {
				log.Printf("post-provision reconcile for pool %q", poolName)
				if err := adapter.Reconcile(); err != nil {
					return err
				}
				return ensureManagedSmoothfsPool(store, poolName)
			})
			zfsHandler.SetAfterPoolImport(func(poolName string) error {
				log.Printf("post-zfs-import reconcile for pool %q", poolName)
				return adapter.Reconcile()
			})
			arraysHandler.SetDestroyPoolNamespaces(func(poolName string) error {
				if err := destroyManagedSmoothfsPool(store, poolName); err != nil {
					log.Printf("destroy pool %s: destroy smoothfs mount: %v", poolName, err)
				}
				nss, err := store.ListMdadmManagedNamespaces()
				if err != nil {
					return err
				}
				for _, ns := range nss {
					if ns.PoolName != poolName {
						continue
					}
					if err := adapter.DestroyNamespace(ns.NamespaceID); err != nil {
						log.Printf("destroy pool %s: destroy ns %s: %v", poolName, ns.NamespaceID, err)
					}
				}
				// Close the meta store too — its bbolt files live on the
				// pool's fastest tier backing, so leaving it open keeps
				// the mount busy and lvremove fails.
				type metaStoreCloser interface {
					ClosePoolMetaStore(poolName string)
				}
				if mc, ok := adapter.(metaStoreCloser); ok {
					mc.ClosePoolMetaStore(poolName)
				}
				return nil
			})
			go resumeManagedSmoothfsPools(store, adapter)
			break
		}
	}
	backupHandler := NewBackupHandler(store)
	arraysHandler.SetPurgeBackupsForPath(backupHandler.PurgeBackupsUnderPath)

	// Authenticated endpoints grouped into a single handler.
	authedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case path == "/api/auth/logout":
			authHandler.Logout(w, r)
		case path == "/api/auth/password":
			authHandler.ChangePassword(w, r)
		case path == "/api/users/me/language":
			userPrefsHandler.Route(w, r)
		case path == "/api/users" || path == "/api/users/":
			switch r.Method {
			case http.MethodGet:
				authHandler.ListUsers(w, r)
			case http.MethodPost:
				authHandler.CreateUser(w, r)
			default:
				jsonMethodNotAllowed(w)
			}
		case strings.HasPrefix(path, "/api/users/"):
			if r.Method == http.MethodDelete {
				authHandler.DeleteUser(w, r, "/api/users/")
			} else {
				jsonMethodNotAllowed(w)
			}
		case strings.HasPrefix(path, "/api/disks"):
			disksHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/smart/"):
			disksHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/arrays"):
			arraysHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/tiers"):
			arraysHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/pools"):
			zfsHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/datasets"):
			zfsHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/zvols"):
			zfsHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/snapshots"):
			zfsHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/protocols"):
			sharingHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/smb/"):
			sharingHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/nfs/"):
			sharingHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/iscsi/"):
			sharingHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/filesystem/"):
			sharingHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/network/"):
			networkHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/benchmark/"):
			benchmarkHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/network-tests/"):
			networkTestsHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/jobs/"):
			jobsHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/system/"):
			systemHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/tiering"):
			tieringHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/smoothfs/pools"):
			smoothfsHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/backup/"):
			backupHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/plugin-profiles"):
			pluginProfilesHandler.Route(w, r)
		case strings.HasPrefix(path, "/api/plugins"):
			pluginsHandler.Route(w, r)
		default:
			jsonNotFound(w)
		}
	})

	terminalHandler := NewTerminalHandler()

	// Wrap authenticated endpoints.
	authWrapped := sgauth.RequireAuth(sessions, authedHandler)
	jsonAuthed := JSONContentType(authWrapped)

	// Terminal WebSocket: authenticated but not JSON-wrapped.
	authTerminal := sgauth.RequireAuth(sessions, terminalHandler)

	// Root mux: unauthenticated routes first, then fall through to authed.
	root := http.NewServeMux()
	root.Handle("/api/health", healthHandler)
	// /api/locale is unauthenticated by design: the login screen needs
	// the installer-chosen language before any user is authenticated.
	root.Handle("/api/locale", NewLocaleHandler())
	root.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		authHandler.Login(w, r)
	})
	root.Handle("/api/terminal", authTerminal)
	root.Handle("/api/", jsonAuthed)

	return root
}
