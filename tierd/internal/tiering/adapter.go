package tiering

// Activity band values. Band assignment is backend-specific; each adapter
// documents its own derivation rules. The control plane must not compare band
// derivation values across adapters.
const (
	ActivityBandHot  = "hot"  // sustained high access rate; fastest-tier candidate
	ActivityBandWarm = "warm" // moderate, intermittent access; intermediate-tier candidate
	ActivityBandCold = "cold" // infrequent access; cold-tier candidate
	ActivityBandIdle = "idle" // no recent access within the collection window
)

// Activity trend values.
const (
	ActivityTrendRising  = "rising"
	ActivityTrendStable  = "stable"
	ActivityTrendFalling = "falling"
)

// IsValidActivityBand reports whether band is a recognised activity band value.
func IsValidActivityBand(band string) bool {
	switch band {
	case ActivityBandHot, ActivityBandWarm, ActivityBandCold, ActivityBandIdle:
		return true
	}
	return false
}

// IsValidActivityTrend reports whether trend is a recognised activity trend value.
func IsValidActivityTrend(trend string) bool {
	switch trend {
	case ActivityTrendRising, ActivityTrendStable, ActivityTrendFalling:
		return true
	}
	return false
}

// TargetSpec is the input to TieringAdapter.CreateTarget.
type TargetSpec struct {
	Name             string
	PlacementDomain  string
	Rank             int
	TargetFillPct    int
	FullThresholdPct int
	BackingRef       string
	BackendDetails   map[string]any
}

// TargetState is the raw per-target state returned by adapter operations.
type TargetState struct {
	ID             string
	Name           string
	CapacityBytes  uint64
	UsedBytes      uint64
	Health         string
	ActivityBand   string
	ActivityTrend  string
	QueueDepth     int
	Capabilities   TargetCapabilities
	BackendDetails map[string]any
}

// TargetPolicy is the fill and threshold policy for a tier target.
type TargetPolicy struct {
	TargetFillPct    int
	FullThresholdPct int
}

// NamespaceSpec is the input to TieringAdapter.CreateNamespace.
type NamespaceSpec struct {
	Name            string
	PlacementDomain string
	NamespaceKind   string // volume | filespace
	ExposedPath     string
	PolicyTargetIDs []string
	BackendDetails  map[string]any
}

// NamespaceState is the raw per-namespace state returned by adapter operations.
type NamespaceState struct {
	ID             string
	Health         string
	PlacementState string
	ActivityBand   string
	BackendRef     string
	BackendDetails map[string]any
}

// ManagedObjectState is returned by TieringAdapter.ListManagedObjects.
type ManagedObjectState struct {
	ID               string
	ObjectKind       string
	ObjectKey        string
	PinState         string
	ActivityBand     string
	PlacementSummary PlacementSummary
	BackendRef       string
	BackendDetails   map[string]any
}

// ActivitySample is a single activity observation returned by CollectActivity.
// The control plane stores these in backend-native tables; the adapter must
// not assume side effects outside its own persistence layer.
type ActivitySample struct {
	TargetID       string
	ObjectID       string
	ActivityBand   string
	ActivityTrend  string
	SampledAt      string
	BackendDetails map[string]any
}

// MovementPlan describes a proposed movement of data between tier targets.
// StartMovement must not block until the background operation completes.
type MovementPlan struct {
	NamespaceID     string
	ObjectID        string // empty for region-granularity movements
	MovementUnit    string
	PlacementDomain string
	SourceTargetID  string
	DestTargetID    string
	PolicyRevision  int64
	IntentRevision  int64
	PlannerEpoch    int64
	TriggeredBy     string
	TotalBytes      int64
}

// MovementState is returned by TieringAdapter.GetMovement.
type MovementState struct {
	ID            string
	State         string // pending | running | completed | failed | cancelled | stale
	ProgressBytes int64
	TotalBytes    int64
	FailureReason string
	StartedAt     string
	UpdatedAt     string
	CompletedAt   string
}

// PinScope specifies the granularity of a pin operation.
type PinScope string

const (
	PinScopeVolume    PinScope = "volume"
	PinScopeNamespace PinScope = "namespace"
	PinScopeObject    PinScope = "object"
	PinScopeNone      PinScope = "none"
)

// BackingSnapshot describes a single ZFS dataset snapshot within a coordinated
// namespace snapshot.
type BackingSnapshot struct {
	DatasetPath  string `json:"dataset_path"`
	SnapshotName string `json:"snapshot_name"`
}

// NamespaceSnapshot is the full record for a coordinated namespace snapshot.
type NamespaceSnapshot struct {
	SnapshotID      string           `json:"snapshot_id"`
	NamespaceID     string           `json:"namespace_id"`
	PoolName        string           `json:"pool_name"`
	ZFSSnapshotName string           `json:"zfs_snapshot_name"`
	BackingSnaps    []BackingSnapshot `json:"backing_snapshots"`
	MetaSnapshot    BackingSnapshot  `json:"metadata_snapshot"`
	CreatedAt       string           `json:"created_at"`
	Consistency     string           `json:"consistency"`
}

// NamespaceSnapshotSummary is the list-view record for a coordinated snapshot.
// It omits BackingSnaps and MetaSnapshot.
type NamespaceSnapshotSummary struct {
	SnapshotID  string `json:"snapshot_id"`
	NamespaceID string `json:"namespace_id"`
	PoolName    string `json:"pool_name"`
	CreatedAt   string `json:"created_at"`
	Consistency string `json:"consistency"`
}

// CoordinatedSnapshotAdapter is implemented by backends that support
// coordinated namespace snapshots (proposal 06).
type CoordinatedSnapshotAdapter interface {
	TieringAdapter

	// CreateNamespaceSnapshot quiesces the namespace, issues an atomic
	// zfs snapshot, persists the record, and returns it.
	// Returns ErrCapabilityViolation when SnapshotMode != coordinated-namespace.
	// Returns ErrTransient when a snapshot is already in progress.
	CreateNamespaceSnapshot(namespaceID string) (*NamespaceSnapshot, error)

	// ListNamespaceSnapshots returns summaries ordered by created_at descending,
	// capped at 50.
	ListNamespaceSnapshots(namespaceID string) ([]NamespaceSnapshotSummary, error)

	// GetNamespaceSnapshot returns the full snapshot record.
	GetNamespaceSnapshot(namespaceID, snapshotID string) (*NamespaceSnapshot, error)

	// DeleteNamespaceSnapshot destroys all backing ZFS snapshots in a single
	// zfs destroy invocation and removes the metadata record.
	DeleteNamespaceSnapshot(namespaceID, snapshotID string) error

	// GetNamespaceSnapshotMode returns "none" or "coordinated-namespace".
	GetNamespaceSnapshotMode(namespaceID string) (string, error)
}

// DegradedState is a degraded-state signal reported by an adapter.
type DegradedState struct {
	ID          string
	BackendKind string
	ScopeKind   string
	ScopeID     string
	Severity    string // warning | critical
	Code        string
	Message     string
	UpdatedAt   string
}

// TieringAdapter is the interface every backend adapter must satisfy to
// participate in the unified tiering control plane.
//
// Methods that start a background operation (e.g. StartMovement) return
// ErrTransient if the backend is temporarily unavailable and ErrPermanent if
// the plan is structurally invalid. They must not block until the background
// operation completes.
//
// CancelMovement must abort an in-progress movement and leave the source
// object authoritative. The adapter is responsible for cleaning up any
// partial copy.
//
// CollectActivity returns samples that the control plane stores in
// backend-native tables; the adapter must not assume side effects outside its
// own persistence layer.
type TieringAdapter interface {
	Kind() string

	// Target lifecycle
	CreateTarget(spec TargetSpec) (*TargetState, error)
	DestroyTarget(targetID string) error
	ListTargets() ([]TargetState, error)

	// Namespace lifecycle
	CreateNamespace(spec NamespaceSpec) (*NamespaceState, error)
	DestroyNamespace(namespaceID string) error
	ListNamespaces() ([]NamespaceState, error)
	ListManagedObjects(namespaceID string) ([]ManagedObjectState, error)

	// Capabilities and policy
	GetCapabilities(targetID string) (TargetCapabilities, error)
	GetPolicy(targetID string) (TargetPolicy, error)
	SetPolicy(targetID string, policy TargetPolicy) error

	// Reconciliation and activity
	Reconcile() error
	CollectActivity() ([]ActivitySample, error)

	// Movement
	PlanMovements() ([]MovementPlan, error)
	StartMovement(plan MovementPlan) (string, error)
	GetMovement(id string) (*MovementState, error)
	CancelMovement(id string) error

	// Pinning
	Pin(scope PinScope, namespaceID string, objectID string) error
	Unpin(scope PinScope, namespaceID string, objectID string) error

	// Degraded state
	GetDegradedState() ([]DegradedState, error)
}
