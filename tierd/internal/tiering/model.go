// Package tiering defines the canonical control-plane model for unified
// storage tiering across all backend adapters. It provides core types,
// the TieringAdapter interface, activity-band constants, and the error
// taxonomy adapters must use when reporting failures to the control plane.
package tiering

// TierTarget is the canonical control-plane representation of one tier target.
// Every backend adapter expresses its storage tiers through this type.
type TierTarget struct {
	ID               string
	Name             string
	PlacementDomain  string
	Rank             int
	BackendKind      string
	TargetFillPct    int
	FullThresholdPct int
	CapacityBytes    uint64
	UsedBytes        uint64
	Health           string
	ActivityBand     string
	ActivityTrend    string
	QueueDepth       int
	Capabilities     TargetCapabilities
	BackingRef       string
	BackendDetails   map[string]any
}

// TargetCapabilities describes the operations a tier target supports.
//
// JSON tags are snake_case so that the serialised form used in
// `tier_targets.capabilities_json` round-trips cleanly with the
// snake_case constants backends emit.
type TargetCapabilities struct {
	MovementGranularity string `json:"movement_granularity"` // region | file | object
	PinScope            string `json:"pin_scope"`            // volume | namespace | object | none
	SupportsOnlineMove  bool   `json:"supports_online_move"`
	SupportsRecall      bool   `json:"supports_recall"`
	RecallMode          string `json:"recall_mode"`   // none | synchronous | asynchronous
	SnapshotMode        string `json:"snapshot_mode"` // none | backend-native | coordinated-namespace
	SupportsChecksums   bool   `json:"supports_checksums"`
	SupportsCompression bool   `json:"supports_compression"`
	SupportsWriteBias   bool   `json:"supports_write_bias"`
}

// ManagedNamespace is a namespace (volume or filespace) under control-plane management.
type ManagedNamespace struct {
	ID              string
	Name            string
	PlacementDomain string
	BackendKind     string
	NamespaceKind   string // volume | filespace
	ExposedPath     string
	PolicyTargetIDs []string
	PinState        string
	Health          string
	ActivityBand    string
	PlacementState  string
	BackendDetails  map[string]any
}

// ManagedObject is an object (volume, file, or object-store entry) within a
// managed namespace.
type ManagedObject struct {
	ID               string
	NamespaceID      string
	ObjectKind       string // volume | file | object
	ObjectKey        string
	PinState         string // none | pinned-hot | pinned-cold
	ActivityBand     string
	PlacementSummary PlacementSummary
	BackendRef       string
	BackendDetails   map[string]any
}

// PlacementSummary describes the current and intended placement of a managed
// object at the control-plane level.
type PlacementSummary struct {
	CurrentTargetID  string
	IntendedTargetID string
	State            string // placed | moving | stale | unknown
}
