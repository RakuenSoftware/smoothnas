package plugin

import (
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

func TestContainerName(t *testing.T) {
	cases := []struct {
		plugin string
		inst   int
		count  int
		want   string
	}{
		{"llama-cpp", 1, 1, "llama-cpp"},
		{"gh-runner", 1, 2, "gh-runner-1"},
		{"gh-runner", 2, 2, "gh-runner-2"},
		{"x", 7, 7, "x-7"},
	}
	for _, tc := range cases {
		// Single-service plugins use the plugin name as the service name.
		if got := ContainerName(tc.plugin, tc.plugin, tc.inst, tc.count); got != tc.want {
			t.Errorf("ContainerName(%q,%d,%d) = %q want %q", tc.plugin, tc.inst, tc.count, got, tc.want)
		}
	}
}

func TestContainerName_MultiService(t *testing.T) {
	// An extra service suffixes the service name onto the plugin base.
	if got := ContainerName("aimee-kb", "postgres", 1, 1); got != "aimee-kb-postgres" {
		t.Errorf("ContainerName multi-service = %q want aimee-kb-postgres", got)
	}
	if got := ContainerName("aimee-kb", "postgres", 2, 3); got != "aimee-kb-postgres-2" {
		t.Errorf("ContainerName multi-service+replica = %q want aimee-kb-postgres-2", got)
	}
}

func TestSetupHash_StableAcrossPackageOrder(t *testing.T) {
	a := SetupHash([]string{"python3", "git"}, []string{"cmd1", "cmd2"})
	b := SetupHash([]string{"git", "python3"}, []string{"cmd1", "cmd2"})
	if a != b {
		t.Errorf("hash should be stable across package order: a=%s b=%s", a, b)
	}
}

func TestSetupHash_ChangesWithSetupReorder(t *testing.T) {
	// Setup script is order-sensitive — same lines in a different
	// order must hash differently.
	a := SetupHash(nil, []string{"useradd worker", "chmod 0700 /home/worker"})
	b := SetupHash(nil, []string{"chmod 0700 /home/worker", "useradd worker"})
	if a == b {
		t.Error("hash should differ when setup line order changes")
	}
}

func TestSetupHash_DifferentInputsDifferentHashes(t *testing.T) {
	a := SetupHash([]string{"python3"}, []string{"echo hi"})
	b := SetupHash([]string{"python3.11"}, []string{"echo hi"})
	if a == b {
		t.Error("hash should differ when packages differ")
	}
}

func TestSetupTemplateImage(t *testing.T) {
	if got := SetupTemplateImage("py-app", "py-app", "0.1.0"); got != "smoothnas-plugin-py-app:0.1.0" {
		t.Errorf("SetupTemplateImage single-service = %q", got)
	}
	if got := SetupTemplateImage("stack", "builder", "0.1.0"); got != "smoothnas-plugin-stack-builder:0.1.0" {
		t.Errorf("SetupTemplateImage multi-service = %q", got)
	}
}

// fakePayloadInputs builds a coherent PayloadInputs starting from a
// fixture. Tests then mutate the result to exercise specific paths.
func fakePayloadInputs(t *testing.T, fixture string) PayloadInputs {
	t.Helper()
	m := mustParse(t, fixture)
	svc := &m.Services[0]

	plugin := PluginRow{
		Name:          m.Metadata.Name,
		Version:       m.Metadata.Version,
		ArtifactType:  svc.Artifact.Type,
		InstanceCount: m.EffectiveCount(),
	}

	volumes := make([]VolumeRow, 0, len(svc.Volumes))
	for _, v := range svc.Volumes {
		row := VolumeRow{
			PluginName:  m.Metadata.Name,
			Service:     svc.Name,
			Name:        v.Name,
			Mode:        v.Mode,
			Slot:        v.Slot,
			BindPath:    v.Bind,
			PerInstance: v.PerInstance,
			Paths:       map[int]string{},
		}
		count := m.EffectiveCount()
		if v.PerInstance {
			for i := 1; i <= count; i++ {
				row.Paths[i] = "/host/" + v.Name + "/inst-" + strFromInt(i)
			}
		} else {
			row.Paths[1] = "/host/" + v.Name
		}
		volumes = append(volumes, row)
	}

	config := make([]ConfigRow, 0, len(svc.Config))
	for _, c := range svc.Config {
		config = append(config, ConfigRow{
			PluginName: m.Metadata.Name,
			Service:    svc.Name,
			Key:        c.Key,
			Value:      c.Default,
		})
	}

	return PayloadInputs{
		Plugin:   &plugin,
		Service:  svc,
		Instance: 1,
		ImageRef: "ghcr.io/example/foo@sha256:" + strings.Repeat("a", 64),
		Volumes:  volumes,
		Config:   config,
	}
}

// strFromInt avoids dragging strconv into the test file just for one
// int → decimal conversion.
func strFromInt(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestBuildCreatePayload_LlamaSingleInstance(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.Image != in.ImageRef {
		t.Errorf("image = %q want %q", got.Image, in.ImageRef)
	}
	wantBind := "/host/models:/models"
	if len(got.HostConfig.Binds) != 1 || got.HostConfig.Binds[0] != wantBind {
		t.Errorf("binds = %v want [%q]", got.HostConfig.Binds, wantBind)
	}
	if got.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("restart policy = %q", got.HostConfig.RestartPolicy.Name)
	}
	if got.Labels[runtime.PluginManagedLabel] != "true" {
		t.Errorf("managed label missing: %+v", got.Labels)
	}
	if got.Labels[runtime.PluginInstanceLabel] != "1" {
		t.Errorf("instance label = %q", got.Labels[runtime.PluginInstanceLabel])
	}
	if got.Labels[runtime.PluginNameLabel] != "llama-cpp" {
		t.Errorf("name label = %q", got.Labels[runtime.PluginNameLabel])
	}
	if got.Labels[runtime.LXC2DockerBindMountInitLabel] != "image" {
		t.Errorf("bind mount init label = %q", got.Labels[runtime.LXC2DockerBindMountInitLabel])
	}
	// Env is sorted; with one MODEL_PATH key we should have exactly one entry.
	if len(got.Env) != 1 || !strings.HasPrefix(got.Env[0], "MODEL_PATH=") {
		t.Errorf("env = %v", got.Env)
	}
}

func TestBuildCreatePayload_HostExposePorts(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Ports = []Port{
		{Name: "http", Port: 8080, Protocol: "tcp", Expose: true},
		{Name: "wolf-control", Port: 47989, Protocol: "tcp", HostExpose: true},
		{Name: "wolf-stream", Port: 47998, Protocol: "udp", HostExpose: true},
	}

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(got.ExposedPorts) != 2 {
		t.Fatalf("ExposedPorts = %+v, want 2 host-exposed ports", got.ExposedPorts)
	}
	if _, ok := got.ExposedPorts["8080/tcp"]; ok {
		t.Errorf("non-hostExpose port should not be in ExposedPorts: %+v", got.ExposedPorts)
	}
	for _, key := range []string{"47989/tcp", "47998/udp"} {
		if _, ok := got.ExposedPorts[key]; !ok {
			t.Errorf("ExposedPorts missing %q: %+v", key, got.ExposedPorts)
		}
		bindings := got.HostConfig.PortBindings[key]
		if len(bindings) != 1 || bindings[0].HostPort != strings.Split(key, "/")[0] {
			t.Errorf("PortBindings[%q] = %+v", key, bindings)
		}
	}
}

func TestBuildCreatePayload_SelectedNVIDIAGPURewritesProfileDevice(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Config = append(in.Service.Config, ConfigField{
		Key:       "SMOOTHNAS_GPU",
		Type:      ConfigTypeGPU,
		GPUVendor: GPUVendorNVIDIA,
	})
	in.Config = append(in.Config, ConfigRow{Key: "SMOOTHNAS_GPU", Value: "/dev/nvidia1"})
	in.Profiles = &Resolved{
		Devices: []ProfileDevice{
			{Path: "/dev/nvidiactl", CgroupPermissions: "rwm"},
			{Path: "/dev/nvidia0", CgroupPermissions: "rwm"},
		},
		LXCRaw: []string{
			"lxc.cgroup2.devices.allow = c 195:* rwm",
			"lxc.mount.entry = /dev/nvidia0 dev/nvidia0 none bind,optional,create=file 0 0",
		},
	}

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(got.HostConfig.Devices) != 2 {
		t.Fatalf("Devices = %+v", got.HostConfig.Devices)
	}
	if got.HostConfig.Devices[1].PathOnHost != "/dev/nvidia1" {
		t.Fatalf("selected GPU was not applied: %+v", got.HostConfig.Devices)
	}
	if got.Labels["io.smoothnas.lxc.raw.1"] != "lxc.mount.entry = /dev/nvidia1 dev/nvidia1 none bind,optional,create=file 0 0" {
		t.Fatalf("raw GPU mount = %q", got.Labels["io.smoothnas.lxc.raw.1"])
	}
}

func TestBuildCreatePayload_SelectedDRIGPURewritesProfileDevice(t *testing.T) {
	// Isolate the render-node rewrite from live sysfs: primary-card-node
	// resolution is covered separately by the test below.
	orig := primaryCardNode
	primaryCardNode = func(string) string { return "" }
	t.Cleanup(func() { primaryCardNode = orig })

	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Config = append(in.Service.Config, ConfigField{
		Key:       "SMOOTHNAS_GPU",
		Type:      ConfigTypeGPU,
		GPUVendor: GPUVendorAMD,
	})
	in.Config = append(in.Config, ConfigRow{Key: "SMOOTHNAS_GPU", Value: "/dev/dri/renderD129"})
	in.Profiles = &Resolved{
		Devices: []ProfileDevice{
			{Path: "/dev/dri", CgroupPermissions: "rwm"},
		},
		LXCRaw: []string{
			"lxc.cgroup2.devices.allow = c 226:* rwm",
			"lxc.mount.entry = /dev/dri dev/dri none bind,optional,create=dir 0 0",
		},
	}

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(got.HostConfig.Devices) != 1 || got.HostConfig.Devices[0].PathOnHost != "/dev/dri/renderD129" {
		t.Fatalf("selected DRI GPU was not applied: %+v", got.HostConfig.Devices)
	}
	if got.Labels["io.smoothnas.lxc.raw.1"] != "lxc.mount.entry = /dev/dri/renderD129 dev/dri/renderD129 none bind,optional,create=file 0 0" {
		t.Fatalf("raw DRI mount = %q", got.Labels["io.smoothnas.lxc.raw.1"])
	}
}

func TestBuildCreatePayload_SelectedDRIGPUAlsoExposesPrimaryCardNode(t *testing.T) {
	orig := primaryCardNode
	primaryCardNode = func(render string) string {
		if render == "/dev/dri/renderD129" {
			return "/dev/dri/card1"
		}
		return ""
	}
	t.Cleanup(func() { primaryCardNode = orig })

	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Config = append(in.Service.Config, ConfigField{
		Key:       "SMOOTHNAS_GPU",
		Type:      ConfigTypeGPU,
		GPUVendor: GPUVendorAMD,
	})
	in.Config = append(in.Config, ConfigRow{Key: "SMOOTHNAS_GPU", Value: "/dev/dri/renderD129"})
	in.Profiles = &Resolved{
		Devices: []ProfileDevice{
			{Path: "/dev/dri", CgroupPermissions: "rwm"},
		},
		LXCRaw: []string{
			"lxc.cgroup2.devices.allow = c 226:* rwm",
			"lxc.mount.entry = /dev/dri dev/dri none bind,optional,create=dir 0 0",
		},
	}

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Both the selected render node and its primary card node must be
	// passed through as devices — Wolf needs the primary node to drive
	// the GPU in app containers.
	devicePaths := map[string]bool{}
	for _, d := range got.HostConfig.Devices {
		devicePaths[d.PathOnHost] = true
	}
	if !devicePaths["/dev/dri/renderD129"] || !devicePaths["/dev/dri/card1"] {
		t.Fatalf("expected render + card node devices, got %+v", got.HostConfig.Devices)
	}

	// And the raw mount entries should carry both nodes.
	var raws []string
	for k, v := range got.Labels {
		if strings.HasPrefix(k, "io.smoothnas.lxc.raw.") {
			raws = append(raws, v)
		}
	}
	joined := strings.Join(raws, "\n")
	if !strings.Contains(joined, "lxc.mount.entry = /dev/dri/renderD129 dev/dri/renderD129") ||
		!strings.Contains(joined, "lxc.mount.entry = /dev/dri/card1 dev/dri/card1") {
		t.Fatalf("expected render + card node raw mounts, got:\n%s", joined)
	}
}

func TestBuildCreatePayload_RejectsInvalidSelectedGPU(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Config = append(in.Service.Config, ConfigField{
		Key:       "SMOOTHNAS_GPU",
		Type:      ConfigTypeGPU,
		GPUVendor: GPUVendorNVIDIA,
	})
	in.Config = append(in.Config, ConfigRow{Key: "SMOOTHNAS_GPU", Value: "/dev/dri/renderD128"})

	if _, err := BuildCreatePayload(in); err == nil {
		t.Fatal("expected invalid GPU selection error")
	}
}

func TestBuildCreatePayload_ExpandsCommandConfigValues(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Container.Command = []string{"--model", "${MODEL_PATH}", "--ctx-size", "$CTX_SIZE", "--keep", "${UNKNOWN}"}
	in.Config = append(in.Config, ConfigRow{PluginName: "llama-cpp", Key: "CTX_SIZE", Value: "131072"})

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := []string{"--model", "/models/default.gguf", "--ctx-size", "131072", "--keep", "${UNKNOWN}"}
	if strings.Join(got.Cmd, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("cmd = %v want %v", got.Cmd, want)
	}
}

func TestBuildCreatePayload_AppliesConfigurableMemoryLimit(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Container.Resources.Memory = "${MEMORY_LIMIT}"
	in.Config = append(in.Config, ConfigRow{PluginName: "llama-cpp", Key: "MEMORY_LIMIT", Value: "64GiB"})

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.HostConfig.Memory != 64<<30 {
		t.Errorf("memory = %d want %d", got.HostConfig.Memory, int64(64<<30))
	}
}

func TestBuildCreatePayload_AppliesConfigurableCPULimit(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Container.Resources.CPU = "${CPU_LIMIT}"
	in.Config = append(in.Config, ConfigRow{PluginName: "llama-cpp", Key: "CPU_LIMIT", Value: "1.5"})

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.HostConfig.NanoCPUs != 1_500_000_000 {
		t.Errorf("NanoCPUs = %d want 1500000000", got.HostConfig.NanoCPUs)
	}
}

func TestBuildCreatePayload_ManifestMemoryOverridesProfileMemory(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Container.Resources.Memory = "64GiB"
	in.Profiles = &Resolved{Memory: 32 << 30, Env: map[string]string{}}

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.HostConfig.Memory != 64<<30 {
		t.Errorf("memory = %d want %d", got.HostConfig.Memory, int64(64<<30))
	}
}

func TestBuildCreatePayload_InvalidMemoryLimitFails(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Container.Resources.Memory = "64XB"

	_, err := BuildCreatePayload(in)
	if err == nil || !strings.Contains(err.Error(), "container.resources.memory") {
		t.Fatalf("err = %v, want container.resources.memory error", err)
	}
}

func TestBuildCreatePayload_InvalidCPULimitFails(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Service.Container.Resources.CPU = "fast"

	_, err := BuildCreatePayload(in)
	if err == nil || !strings.Contains(err.Error(), "container.resources.cpu") {
		t.Fatalf("err = %v, want container.resources.cpu error", err)
	}
}

func TestBuildCreatePayload_MultiInstancePerInstanceVolume(t *testing.T) {
	in := fakePayloadInputs(t, "gh-runner.yaml")

	// Render for instance 1.
	in.Instance = 1
	got1, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build inst 1: %v", err)
	}
	// Render for instance 2.
	in.Instance = 2
	got2, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build inst 2: %v", err)
	}

	if got1.HostConfig.Binds[0] == got2.HostConfig.Binds[0] {
		t.Errorf("perInstance volume should differ between instances: 1=%q 2=%q",
			got1.HostConfig.Binds[0], got2.HostConfig.Binds[0])
	}
	if got1.Labels[runtime.PluginInstanceLabel] != "1" || got2.Labels[runtime.PluginInstanceLabel] != "2" {
		t.Errorf("instance labels: 1=%q 2=%q",
			got1.Labels[runtime.PluginInstanceLabel], got2.Labels[runtime.PluginInstanceLabel])
	}
}

func TestBuildCreatePayload_UnresolvedTierBoundVolumeFails(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	// Simulate phase-3 not having resolved the path yet.
	in.Volumes[0].Paths[1] = ""

	_, err := BuildCreatePayload(in)
	if err == nil {
		t.Fatal("expected error for unresolved tier-bound volume")
	}
	if !strings.Contains(err.Error(), "host_path for instance 1") {
		t.Errorf("error %q should mention the instance", err)
	}
}

func TestBuildCreatePayload_RejectsEmptyImageRef(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.ImageRef = ""
	if _, err := BuildCreatePayload(in); err == nil {
		t.Error("expected error on empty image ref")
	}
}
