// Package backend defines the pluggable per-tier storage provisioner.
//
// A tier slot can be backed by any filesystem that offers a mountable
// data area. Each concrete implementation lives in its own file and
// registers itself in the Backends map at package init. The tier
// manager dispatches Provision / Destroy by looking up the slot's
// backing_kind.
//
// Adding a new kind (btrfs, bcachefs, …) is intentionally small:
//   1. Add the kind string to the CHECK constraint in migrations.
//   2. Implement Backend for that kind in this package.
//   3. Register it in init().
// No other tierd code should branch on kind.
package backend

import "fmt"

// ProvisionOpts is the per-call tuning the caller hands to a backend
// when preparing a tier slot. Fields are kind-specific; backends that
// don't care about a field just ignore it.
type ProvisionOpts struct {
	// Filesystem picks the on-disk format when the backend has to
	// mkfs a raw block device (mdadm). ZFS/btrfs/bcachefs bring
	// their own filesystem and ignore this.
	Filesystem string
}

// Backend creates and tears down the filesystem that backs one tier
// slot. Implementations are stateless — the tier manager passes all
// required identity (pool, tier, ref) and the target mount point on
// every call.
type Backend interface {
	// Kind returns the identifier used in tier_pools.backing_kind.
	Kind() string

	// Provision prepares the tier's data area on top of `ref` and
	// makes `mountPoint` a live filesystem. Implementations MUST be
	// idempotent — tierd re-runs Provision on every boot.
	//
	// ref is the kind-specific handle stored in tiers.backing_ref
	// (e.g. "/dev/md0" for mdadm, "tank" for zfs).
	Provision(poolName, tierName, ref, mountPoint string, opts ProvisionOpts) error

	// Destroy tears down the backing. May be called after a reboot
	// where the process state has been lost; implementations should
	// no-op gracefully on "already destroyed" inputs.
	Destroy(poolName, tierName, ref, mountPoint string) error
}

// MetaLVProvider carves out the per-tier metadata LV on mdadm-backed
// pools. Injected at startup by the tier.Manager so the mdadm backend
// can keep it mdadm-specific without dragging the tiermeta package
// into this import graph.
type MetaLVProvider interface {
	CreateSlotMetaLV(poolName, tierName, pvDevice string, pvSizeBytes uint64) error
}

// mdadmMetaProvider is the currently-registered meta-LV carver. Nil
// is valid — the mdadm backend treats meta-LV creation as best-effort.
var mdadmMetaProvider MetaLVProvider

// SetMdadmMetaProvider wires a provider into the mdadm backend. Safe
// to call before any Provision runs; the tier.Manager does it during
// startup as part of its NewManager chain.
func SetMdadmMetaProvider(p MetaLVProvider) {
	mdadmMetaProvider = p
}

// Backends is the registry, keyed by Kind(). Populated from each
// backend's init() so adding a file is enough to add a kind.
var Backends = map[string]Backend{}

// Register installs a backend in the registry. Call from init().
// Panics on duplicate kinds — two backends fighting for the same
// kind string is always a bug worth stopping the daemon for.
func Register(b Backend) {
	k := b.Kind()
	if _, ok := Backends[k]; ok {
		panic(fmt.Sprintf("tier backend %q already registered", k))
	}
	Backends[k] = b
}

// Lookup returns the backend for a kind, or an error if none is
// registered. "mdadm" is resolved as the default when kind is empty
// to stay compatible with rows migrated from the pre-backing-kind
// schema where the column didn't exist yet.
func Lookup(kind string) (Backend, error) {
	if kind == "" {
		kind = "mdadm"
	}
	b, ok := Backends[kind]
	if !ok {
		return nil, fmt.Errorf("no tier backend registered for kind %q", kind)
	}
	return b, nil
}
