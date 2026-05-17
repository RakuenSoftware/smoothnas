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
		if got := ContainerName(tc.plugin, tc.inst, tc.count); got != tc.want {
			t.Errorf("ContainerName(%q,%d,%d) = %q want %q", tc.plugin, tc.inst, tc.count, got, tc.want)
		}
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
	if got := SetupTemplateImage("py-app", "0.1.0"); got != "smoothnas-plugin-py-app:0.1.0" {
		t.Errorf("SetupTemplateImage = %q", got)
	}
}

// fakePayloadInputs builds a coherent PayloadInputs starting from a
// fixture. Tests then mutate the result to exercise specific paths.
func fakePayloadInputs(t *testing.T, fixture string) PayloadInputs {
	t.Helper()
	m := mustParse(t, fixture)

	plugin := PluginRow{
		Name:          m.Metadata.Name,
		Version:       m.Metadata.Version,
		ArtifactType:  m.Artifact.Type,
		InstanceCount: m.EffectiveCount(),
	}

	volumes := make([]VolumeRow, 0, len(m.Volumes))
	for _, v := range m.Volumes {
		row := VolumeRow{
			PluginName:  m.Metadata.Name,
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

	config := make([]ConfigRow, 0, len(m.Config))
	for _, c := range m.Config {
		config = append(config, ConfigRow{
			PluginName: m.Metadata.Name,
			Key:        c.Key,
			Value:      c.Default,
		})
	}

	return PayloadInputs{
		Plugin:   &plugin,
		Manifest: m,
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
	in.Manifest.Ports = []Port{
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

func TestBuildCreatePayload_ExpandsCommandConfigValues(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Manifest.Container.Command = []string{"--model", "${MODEL_PATH}", "--ctx-size", "$CTX_SIZE", "--keep", "${UNKNOWN}"}
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
	in.Manifest.Container.Resources.Memory = "${MEMORY_LIMIT}"
	in.Config = append(in.Config, ConfigRow{PluginName: "llama-cpp", Key: "MEMORY_LIMIT", Value: "64GiB"})

	got, err := BuildCreatePayload(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.HostConfig.Memory != 64<<30 {
		t.Errorf("memory = %d want %d", got.HostConfig.Memory, int64(64<<30))
	}
}

func TestBuildCreatePayload_ManifestMemoryOverridesProfileMemory(t *testing.T) {
	in := fakePayloadInputs(t, "llama.yaml")
	in.Manifest.Container.Resources.Memory = "64GiB"
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
	in.Manifest.Container.Resources.Memory = "64XB"

	_, err := BuildCreatePayload(in)
	if err == nil || !strings.Contains(err.Error(), "container.resources.memory") {
		t.Fatalf("err = %v, want container.resources.memory error", err)
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
