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
	healthHandler := health.NewHandler(version, startTime)
	sessions := sgauth.NewSessionStore(store.DB(), 24*time.Hour)
	rateLimiter := sgauth.NewRateLimiter(store.DB(), 5, 15*time.Minute)
	users := sgauth.NewUserManager("tierd")
	authHandler := sgauth.NewHandler("tierd", sessions, rateLimiter, users)
	disksHandler := NewDisksHandler(historyStore, alarmStore)
	arraysHandler := NewArraysHandler(store)
	arraysHandler.ResumeDestroyingPools()
	zfsHandler := NewZFSHandler()
	sharingHandler := NewSharingHandler(store)
	networkHandler := NewNetworkHandler()
	benchmarkHandler := NewBenchmarkHandler()
	networkTestsHandler := NewNetworkTestsHandler()
	upd := updater.New(version)
	systemHandler := NewSystemHandler(mon, upd)
	jobsHandler := NewJobsHandler()
	tieringHandler := NewTieringHandler(store)
	for _, a := range adapters {
		if err := tieringHandler.RegisterAdapter(a); err != nil {
			log.Printf("tiering: skipping adapter %q: %v", a.Kind(), err)
		}
	}
	// Wire automatic FUSE namespace creation: after per-tier provisioning
	// succeeds the handler triggers adapter reconciliation which ensures
	// tier targets, managed targets, namespace, and FUSE daemon all exist.
	for _, a := range adapters {
		if a.Kind() == "mdadm" {
			adapter := a
			arraysHandler.SetEnsureNamespace(func(poolName string) error {
				log.Printf("post-provision reconcile for pool %q", poolName)
				return adapter.Reconcile()
			})
			arraysHandler.SetDestroyPoolNamespaces(func(poolName string) error {
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
		case path == "/api/users" || path == "/api/users/":
			switch r.Method {
			case http.MethodGet:
				authHandler.ListUsers(w, r)
			case http.MethodPost:
				authHandler.CreateUser(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		case strings.HasPrefix(path, "/api/users/"):
			if r.Method == http.MethodDelete {
				authHandler.DeleteUser(w, r, "/api/users/")
			} else {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
		case strings.HasPrefix(path, "/api/backup/"):
			backupHandler.Route(w, r)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
	root.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		authHandler.Login(w, r)
	})
	root.Handle("/api/terminal", authTerminal)
	root.Handle("/api/", jsonAuthed)

	return root
}
