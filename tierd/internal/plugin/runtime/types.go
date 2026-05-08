package runtime

// This file declares the JSON shapes tierd exchanges with the daemon.
// Field names match the Docker Engine API; field sets are the subset
// tierd actually reads or writes. Adding a field here is cheap; we
// just have to be careful that omitted fields don't accidentally take
// effect (most Docker fields default to "do nothing", so this is
// rarely a concern).

// CreateContainerRequest is the body of POST /containers/create.
// LXC2Docker accepts the same JSON shape Docker proper does.
type CreateContainerRequest struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// HostConfig is the subset of HostConfig tierd populates. The Docker
// API has dozens of fields here; we only set the ones the plugin
// system actually needs in this phase.
type HostConfig struct {
	Binds         []string         `json:"Binds,omitempty"`         // "<host_path>:<bind_path>[:ro]"
	NetworkMode   string           `json:"NetworkMode,omitempty"`   // "smoothnas-plugins" once phase 04 lands
	RestartPolicy RestartPolicy    `json:"RestartPolicy"`
	Devices       []DeviceMapping  `json:"Devices,omitempty"`       // populated by phase 05 (profiles)
	CapAdd        []string         `json:"CapAdd,omitempty"`        // capabilities a profile can grant
	PidsLimit     int64            `json:"PidsLimit,omitempty"`     // 0 = unlimited; profiles can cap
	OomScoreAdj   int              `json:"OomScoreAdj,omitempty"`   // -1000..1000; default-limits sets 100
	PortBindings  map[string][]PortBinding `json:"PortBindings,omitempty"` // empty for v1; phase 09 may populate
}

// RestartPolicy maps the manifest's container.restartPolicy to
// Docker's wire shape.
type RestartPolicy struct {
	Name              string `json:"Name"`              // "no" | "on-failure" | "unless-stopped" | "always"
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

// DeviceMapping is one entry in HostConfig.Devices. Phase 05 (profiles)
// populates these for GPU passthrough.
type DeviceMapping struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"` // "rwm" typical
}

// PortBinding is a host-side binding for a container port. Reserved
// for phase 09; tierd does not populate these in phase 02.
type PortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort"` // string per Docker's wire shape
}

// CreateContainerResponse is the body returned by POST /containers/create.
type CreateContainerResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// ContainerInspect is the subset of GET /containers/{id}/json tierd
// reads. Lifecycle code uses State + NetworkSettings; reconciliation
// uses ID + Name + Labels.
type ContainerInspect struct {
	ID              string                            `json:"Id"`
	Name            string                            `json:"Name"`     // includes leading "/"
	Image           string                            `json:"Image"`    // resolved image ref
	State           ContainerState                    `json:"State"`
	Config          ContainerConfig                   `json:"Config"`
	HostConfig      HostConfig                        `json:"HostConfig"`
	NetworkSettings ContainerNetworkSettings          `json:"NetworkSettings"`
}

// ContainerState mirrors Docker's State block.
type ContainerState struct {
	Status     string `json:"Status"`     // "created" | "running" | "exited" | "restarting" | "paused" | "dead"
	Running    bool   `json:"Running"`
	Restarting bool   `json:"Restarting"`
	OOMKilled  bool   `json:"OOMKilled"`
	Dead       bool   `json:"Dead"`
	Pid        int    `json:"Pid"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

// ContainerConfig is the subset of the Config block tierd reads back.
type ContainerConfig struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

// ContainerNetworkSettings exposes the bridge IP phase 04's nginx
// proxy needs. The map key is the network name (`smoothnas-plugins`
// once phase 04 lands; `bridge` before that).
type ContainerNetworkSettings struct {
	Networks map[string]ContainerNetwork `json:"Networks"`
}

// ContainerNetwork is one entry under NetworkSettings.Networks.
type ContainerNetwork struct {
	IPAddress  string `json:"IPAddress"`
	Gateway    string `json:"Gateway"`
	MacAddress string `json:"MacAddress"`
}

// ContainerSummary is one entry in GET /containers/json (the list
// endpoint). Used by the reconciler to enumerate managed containers
// at startup.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`  // "created" | "running" | ...
	Status string            `json:"Status"` // human-readable, "Up 3 hours"
	Labels map[string]string `json:"Labels"`
}

// PluginManagedLabel is the marker tierd writes onto every container
// it owns. The reconciler filters on this label so it doesn't
// accidentally see operator-installed containers.
const PluginManagedLabel = "io.smoothnas.managed"

// PluginNameLabel is the plugin name as recorded in the plugins table.
const PluginNameLabel = "io.smoothnas.plugin"

// PluginVersionLabel is the manifest version.
const PluginVersionLabel = "io.smoothnas.plugin.version"

// PluginInstanceLabel is the per-instance index (1..N), as a decimal
// string. Used by the event subscriber to route events back to the
// right plugin_instances row.
const PluginInstanceLabel = "io.smoothnas.plugin.instance"
