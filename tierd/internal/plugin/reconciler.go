package plugin

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

// Reconciler keeps tierd's view of the plugin runtime in sync with
// the daemon. At tierd startup it pings the daemon, walks every
// plugin_instances row, and reconciles state. At runtime it
// subscribes to the daemon's event stream so state changes from
// outside tierd (operator-issued docker stop, OOM, daemon crash) are
// reflected promptly.
type Reconciler struct {
	store *Store
	rt    RuntimeClient
}

// NewReconciler constructs a Reconciler. The runtime client is the
// same one Lifecycle uses; both are read/write on plugin_instances.
func NewReconciler(s *Store, rt RuntimeClient) *Reconciler {
	return &Reconciler{store: s, rt: rt}
}

// Sync runs a full reconciliation pass: ping the daemon, list every
// managed container, then for each plugin_instances row check that
// the recorded container still exists and its state matches what
// the daemon reports. Containers tierd doesn't know about are
// logged as ghosts.
//
// Idempotent — safe to call repeatedly. Phase 02 calls this exactly
// once at tierd startup.
func (r *Reconciler) Sync(ctx context.Context) error {
	if err := r.rt.Ping(ctx); err != nil {
		return fmt.Errorf("runtime not reachable: %w", err)
	}

	managed, err := r.rt.ListManagedContainers(ctx)
	if err != nil {
		return fmt.Errorf("list managed containers: %w", err)
	}
	// Index by container ID for fast lookup.
	managedByID := make(map[string]runtime.ContainerSummary, len(managed))
	for _, c := range managed {
		managedByID[c.ID] = c
	}

	// Walk every plugin and reconcile its instances.
	plugins, err := r.store.List()
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}
	seenContainers := make(map[string]bool)

	for _, p := range plugins {
		rec, err := r.store.Get(p.Name)
		if err != nil {
			return err
		}
		for _, inst := range rec.Instances {
			if inst.ContainerID == "" {
				// Phase-1 install hasn't been materialised yet —
				// nothing to reconcile.
				continue
			}
			seenContainers[inst.ContainerID] = true

			summary, ok := managedByID[inst.ContainerID]
			if !ok {
				// DB says we own this container but the daemon doesn't
				// know it. Mark the instance failed so the operator
				// sees it; the lifecycle will recreate on the next
				// Materialise.
				_ = r.store.SetInstanceState(p.Name, inst.Instance, StateFailed,
					"container missing from runtime at startup")
				log.Printf("plugin reconcile: %s instance %d missing container %s",
					p.Name, inst.Instance, shortID(inst.ContainerID))
				continue
			}
			// Update state from runtime view.
			newState := mapDockerState(summary.State)
			if newState != inst.State {
				_ = r.store.SetInstanceState(p.Name, inst.Instance, newState, "")
			}
			// Refresh bridge IP — it's stable across restarts but not
			// across recreate.
			if inst.BridgeIP == "" {
				if details, err := r.rt.InspectContainer(ctx, inst.ContainerID); err == nil {
					if ip := pickBridgeIP(details); ip != "" {
						_ = r.store.SetInstanceBridgeIP(p.Name, inst.Instance, ip)
					}
				}
			}
		}
	}

	// Ghost containers — daemon has them but tierd's DB does not.
	// We don't delete them automatically; that's a destructive call
	// the operator should make. Log them so a human notices.
	for id, summary := range managedByID {
		if seenContainers[id] {
			continue
		}
		pluginName := summary.Labels[runtime.PluginNameLabel]
		log.Printf("plugin reconcile: ghost container %s (plugin=%q) — operator action required",
			shortID(id), pluginName)
	}

	return nil
}

// WatchEvents subscribes to the daemon's event stream and updates
// the DB on every container state transition. Call from a goroutine
// at tierd startup; returns when ctx is cancelled. Reconnects
// automatically with backoff if the stream drops.
func (r *Reconciler) WatchEvents(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.watchOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("plugin event stream: %v; reconnecting in %v", err, runtime.EventStreamReconnectInterval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(runtime.EventStreamReconnectInterval):
			}
		}
	}
}

func (r *Reconciler) watchOnce(ctx context.Context) error {
	events, errs, err := r.rt.SubscribeEvents(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				// Stream ended.
				select {
				case e := <-errs:
					return e
				default:
					return nil
				}
			}
			r.handleEvent(ev)
		}
	}
}

// handleEvent updates plugin_instances based on a single Docker
// event. Routing uses the io.smoothnas.plugin and io.smoothnas.plugin.instance
// labels the lifecycle wrote at create time.
func (r *Reconciler) handleEvent(ev runtime.Event) {
	if ev.Type != "container" {
		return
	}
	pluginName := ev.Actor.Attributes[runtime.PluginNameLabel]
	instStr := ev.Actor.Attributes[runtime.PluginInstanceLabel]
	if pluginName == "" || instStr == "" {
		return // not a managed plugin event
	}
	instance, err := strconv.Atoi(instStr)
	if err != nil {
		return
	}
	state, lastError := mapDockerEventToState(ev.Action)
	if state == "" {
		return // an event we don't translate (e.g. exec_create)
	}
	if err := r.store.SetInstanceState(pluginName, instance, state, lastError); err != nil {
		log.Printf("plugin event: update %s/%d → %s: %v", pluginName, instance, state, err)
	}
}

// mapDockerState maps Docker's `State` string from a list/inspect
// response to tierd's per-instance state.
func mapDockerState(s string) string {
	switch s {
	case "running":
		return StateRunning
	case "created", "exited":
		return StateStopped
	case "restarting":
		return StateStarting
	case "paused":
		return StateStopped
	case "dead":
		return StateFailed
	}
	return StateInstalled
}

// mapDockerEventToState maps a Docker event Action to a (state, error)
// pair. Returns ("", "") for events we don't translate so handleEvent
// can no-op cleanly.
func mapDockerEventToState(action string) (state, lastErr string) {
	switch action {
	case "start":
		return StateRunning, ""
	case "die":
		return StateStopped, ""
	case "oom":
		return StateFailed, "OOM"
	case "destroy":
		return StateStopped, ""
	}
	return "", ""
}

// pickBridgeIP returns the first non-empty IP across all attached
// networks. Phase 04 will introduce a managed `smoothnas-plugins`
// bridge and the picker can be made specific to that name; phase 02
// just takes whatever the daemon gave us.
func pickBridgeIP(details runtime.ContainerInspect) string {
	for _, net := range details.NetworkSettings.Networks {
		if net.IPAddress != "" {
			return net.IPAddress
		}
	}
	return ""
}

// shortID truncates a container ID for log output.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
