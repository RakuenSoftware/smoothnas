package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFakeRuntime spins up an HTTP server bound to a unix socket
// inside t.TempDir() and returns the socket path. The server is
// torn down by t.Cleanup. Tests pass the socket path to NewClient.
//
// Handlers are routed through a single http.Handler so tests can
// inspect request paths/methods/bodies inline.
func newFakeRuntime(t *testing.T, h http.Handler) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(listener) //nolint:errcheck // closes on cleanup

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = listener.Close()
	})
	return socketPath
}

func TestPing_OK(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_ping" {
			t.Errorf("path = %q want /_ping", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	c := NewClient(sock)
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("ping: %v", err)
	}
}

func TestPing_DaemonDown(t *testing.T) {
	c := NewClient("/nonexistent/socket")
	err := c.Ping(context.Background())
	if err == nil {
		t.Error("expected error talking to nonexistent socket")
	}
}

func TestWaitForReady_ReturnsWhenSocketComesUp(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	c := NewClient(socketPath)

	ready := make(chan struct{})
	go func() {
		// Bring up the socket after a short delay.
		time.Sleep(150 * time.Millisecond)
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Errorf("listen: %v", err)
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})}
		go srv.Serve(listener) //nolint:errcheck
		close(ready)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
			_ = listener.Close()
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitForReady(ctx); err != nil {
		t.Errorf("wait ready: %v", err)
	}
	<-ready
}

func TestWaitForReady_ContextCancel(t *testing.T) {
	c := NewClient("/nonexistent/socket")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := c.WaitForReady(ctx)
	if err == nil {
		t.Error("expected error on context expiry")
	}
}

func TestInfo_OK(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Info{
			ServerVersion: "lxc2docker-0.0.1",
			NCPU:          8,
			MemTotal:      32 << 30,
		})
	}))
	c := NewClient(sock)
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.ServerVersion != "lxc2docker-0.0.1" || info.NCPU != 8 {
		t.Errorf("info = %+v", info)
	}
}

func TestAPIError_4xxIncludesDaemonMessage(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "no such container: bogus"})
	}))
	c := NewClient(sock)
	_, err := c.InspectContainer(context.Background(), "bogus")
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", ae.StatusCode)
	}
	if ae.Message != "no such container: bogus" {
		t.Errorf("message = %q", ae.Message)
	}
	if !IsNotFound(err) {
		t.Error("IsNotFound should be true")
	}
}

func TestCreateContainer_BodyShape(t *testing.T) {
	var body CreateContainerRequest
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/create" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("name"); got != "llama-cpp" {
			t.Errorf("name query = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(CreateContainerResponse{ID: "abc123"})
	}))
	c := NewClient(sock)
	resp, err := c.CreateContainer(context.Background(), "llama-cpp", CreateContainerRequest{
		Image: "ghcr.io/foo/bar:1",
		Cmd:   []string{"./run"},
		Env:   []string{"X=1"},
		Labels: map[string]string{
			PluginManagedLabel:  "true",
			PluginNameLabel:     "llama-cpp",
			PluginInstanceLabel: "1",
		},
		ExposedPorts: map[string]struct{}{
			"47989/tcp": {},
			"47998/udp": {},
		},
		HostConfig: HostConfig{
			Binds:         []string{"/host:/container"},
			Memory:        64 << 30,
			RestartPolicy: RestartPolicy{Name: "unless-stopped"},
			PortBindings: map[string][]PortBinding{
				"47989/tcp": []PortBinding{{HostPort: "47989"}},
				"47998/udp": []PortBinding{{HostPort: "47998"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.ID != "abc123" {
		t.Errorf("id = %q", resp.ID)
	}
	if body.Image != "ghcr.io/foo/bar:1" || body.HostConfig.Binds[0] != "/host:/container" {
		t.Errorf("body roundtrip wrong: %+v", body)
	}
	if body.Labels[PluginManagedLabel] != "true" {
		t.Errorf("managed label not propagated: %+v", body.Labels)
	}
	if body.HostConfig.Memory != 64<<30 {
		t.Errorf("memory = %d", body.HostConfig.Memory)
	}
	if _, ok := body.ExposedPorts["47989/tcp"]; !ok {
		t.Errorf("ExposedPorts missing tcp binding: %+v", body.ExposedPorts)
	}
	if bindings := body.HostConfig.PortBindings["47998/udp"]; len(bindings) != 1 || bindings[0].HostPort != "47998" {
		t.Errorf("udp PortBindings = %+v", bindings)
	}
}

func TestStartContainer_IdempotentOnConflict(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "container already started"})
	}))
	c := NewClient(sock)
	if err := c.StartContainer(context.Background(), "abc"); err != nil {
		t.Errorf("start should be idempotent on 409: %v", err)
	}
}

func TestRemoveContainer_IdempotentOnNotFound(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	c := NewClient(sock)
	if err := c.RemoveContainer(context.Background(), "abc", true); err != nil {
		t.Errorf("remove should be idempotent on 404: %v", err)
	}
}

func TestRemoveImage_IdempotentOnNotFound(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	c := NewClient(sock)
	if err := c.RemoveImage(context.Background(), "ghcr.io/foo/bar:1"); err != nil {
		t.Errorf("remove should be idempotent on 404: %v", err)
	}
}

func TestListManagedContainers_FilterEncoding(t *testing.T) {
	var seenFilters string
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenFilters = r.URL.Query().Get("filters")
		_ = json.NewEncoder(w).Encode([]ContainerSummary{
			{
				ID:    "c1",
				Names: []string{"/llama-cpp"},
				State: "running",
				Labels: map[string]string{
					PluginManagedLabel: "true",
					PluginNameLabel:    "llama-cpp",
				},
			},
		})
	}))
	c := NewClient(sock)
	out, err := c.ListManagedContainers(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 1 || out[0].Labels[PluginNameLabel] != "llama-cpp" {
		t.Errorf("list result = %+v", out)
	}
	// Verify the filter is well-formed JSON containing our managed label.
	var parsed map[string][]string
	if err := json.Unmarshal([]byte(seenFilters), &parsed); err != nil {
		t.Errorf("filters not valid JSON: %v (raw=%q)", err, seenFilters)
	}
	if !contains(parsed["label"], PluginManagedLabel+"=true") {
		t.Errorf("filter missing managed label: %+v", parsed)
	}
}

func TestListContainers_UnfilteredAllContainers(t *testing.T) {
	var seenAll string
	var seenFilters string
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAll = r.URL.Query().Get("all")
		seenFilters = r.URL.Query().Get("filters")
		_ = json.NewEncoder(w).Encode([]ContainerSummary{
			{ID: "managed", Labels: map[string]string{PluginManagedLabel: "true"}},
			{ID: "worker", Labels: map[string]string{"io.smoothnas.gh-runner.worker": "true"}},
		})
	}))
	c := NewClient(sock)
	out, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("containers = %d want 2", len(out))
	}
	if seenAll != "1" {
		t.Errorf("all = %q want 1", seenAll)
	}
	if seenFilters != "" {
		t.Errorf("filters = %q want empty", seenFilters)
	}
}

func TestPullImage_StreamingAndDigestExtraction(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/create" {
			http.NotFound(w, r)
			return
		}
		// Simulate the daemon's line-delimited JSON event stream.
		w.Header().Set("Content-Type", "application/json")
		flusher, _ := w.(http.Flusher)
		events := []map[string]any{
			{"status": "Pulling from foo/bar", "id": "1.2.3"},
			{"status": "Pulling fs layer", "id": "abc"},
			{"status": "Downloading", "progressDetail": map[string]any{"current": 1024, "total": 4096}},
			{"status": "Status: Downloaded newer image for foo/bar:1.2.3"},
			{"status": "Digest: sha256:" + strings.Repeat("a", 64)},
		}
		for _, ev := range events {
			b, _ := json.Marshal(ev)
			fmt.Fprintln(w, string(b))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	c := NewClient(sock)

	var seen int32
	resolved, err := c.PullImage(context.Background(), "foo/bar:1.2.3", func(_ PullEvent) {
		atomic.AddInt32(&seen, 1)
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if want := "foo/bar@sha256:" + strings.Repeat("a", 64); resolved != want {
		t.Errorf("resolved = %q want %q", resolved, want)
	}
	if got := atomic.LoadInt32(&seen); got != 5 {
		t.Errorf("progress callback fired %d times, want 5", got)
	}
}

func TestPullImage_ErrorEventReturnsErr(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "manifest unknown"})
	}))
	c := NewClient(sock)
	_, err := c.PullImage(context.Background(), "ghcr.io/foo/bar:nope", nil)
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("expected manifest-unknown error, got %v", err)
	}
}

func TestPullImage_NoDigestEventReturnsRefUnchanged(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "Pulled distro template"})
	}))
	c := NewClient(sock)
	got, err := c.PullImage(context.Background(), "ubuntu:jammy", nil)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got != "ubuntu:jammy" {
		t.Errorf("resolved = %q want ubuntu:jammy (no digest, lxc-distro path)", got)
	}
}

func TestSubscribeEvents_DeliversThenCloses(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		flusher, _ := w.(http.Flusher)
		evs := []Event{
			{Type: "container", Action: "start", Actor: EventActor{ID: "c1", Attributes: map[string]string{PluginNameLabel: "llama-cpp"}}},
			{Type: "container", Action: "die", Actor: EventActor{ID: "c1", Attributes: map[string]string{PluginNameLabel: "llama-cpp"}}},
		}
		for _, ev := range evs {
			b, _ := json.Marshal(ev)
			fmt.Fprintln(w, string(b))
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Then close the response by returning.
	}))
	c := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, errs, err := c.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var got []Event
	for ev := range events {
		got = append(got, ev)
	}
	// Drain errs to avoid leaking the goroutine in a real test scenario.
	select {
	case <-errs:
	default:
	}
	if len(got) != 2 || got[0].Action != "start" || got[1].Action != "die" {
		t.Errorf("events = %+v", got)
	}
}

func TestStreamLogs_ReturnsBody(t *testing.T) {
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/abc/logs" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("line one\nline two\n"))
	}))
	c := NewClient(sock)
	rc, err := c.StreamLogs(context.Background(), "abc", LogsOptions{Stdout: true, Stderr: true})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "line one\nline two\n" {
		t.Errorf("body = %q", string(body))
	}
}

func TestSplitRef_Cases(t *testing.T) {
	cases := []struct{ in, wantFrom, wantTag string }{
		{"ubuntu", "ubuntu", "latest"},
		{"ubuntu:22.04", "ubuntu", "22.04"},
		{"ghcr.io/foo/bar:1.2.3", "ghcr.io/foo/bar", "1.2.3"},
		{"registry:5000/foo", "registry:5000/foo", "latest"},
		{"registry:5000/foo:tag", "registry:5000/foo", "tag"},
		{"foo/bar@sha256:" + strings.Repeat("a", 64), "foo/bar@sha256:" + strings.Repeat("a", 64), ""},
	}
	for _, tc := range cases {
		gotFrom, gotTag := splitRef(tc.in)
		if gotFrom != tc.wantFrom || gotTag != tc.wantTag {
			t.Errorf("splitRef(%q) = (%q,%q) want (%q,%q)", tc.in, gotFrom, gotTag, tc.wantFrom, tc.wantTag)
		}
	}
}

func TestExtractDigest(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Digest: sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("a", 64)},
		{"Digest: sha256:" + strings.Repeat("b", 64) + "\nStatus: ok", "sha256:" + strings.Repeat("b", 64)},
		{"Status: Downloaded newer image", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := extractDigest(tc.in); got != tc.want {
			t.Errorf("extractDigest(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestDoEncodesQueryString(t *testing.T) {
	var seen url.Values
	sock := newFakeRuntime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	c := NewClient(sock)
	_, err := c.do(context.Background(), http.MethodPost, "/containers/abc/stop", url.Values{"t": []string{"5"}}, nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if seen.Get("t") != "5" {
		t.Errorf("query t = %q", seen.Get("t"))
	}
}

// contains is a tiny helper for the filter-encoding test.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
