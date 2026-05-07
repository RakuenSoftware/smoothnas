package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeReloader records every Reload call and optionally fails on
// demand. Used to assert that Apply / Remove invoke the reloader at
// the right moments.
type fakeReloader struct {
	mu       sync.Mutex
	calls    int
	err      error
	failOnce bool // when true, the next call fails and the rest succeed
}

func (f *fakeReloader) Reload(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failOnce {
		f.failOnce = false
		return errors.New("nginx -t failed: invalid syntax (simulated)")
	}
	return f.err
}

// newTestProxy constructs a Proxy rooted at t.TempDir() so writes go
// to a tempdir instead of /etc/nginx.
func newTestProxy(t *testing.T) (*Proxy, *fakeReloader, string) {
	t.Helper()
	root := t.TempDir()
	confDir := filepath.Join(root, "plugins.d")
	baseConf := filepath.Join(root, "smoothnas-plugins-base.conf")
	rl := &fakeReloader{}
	p := NewProxy()
	p.SetConfDir(confDir)
	p.SetBaseConfPath(baseConf)
	p.SetReloader(rl)
	return p, rl, confDir
}

func TestProxy_Apply_WritesPerPluginConfAndReloads(t *testing.T) {
	p, rl, confDir := newTestProxy(t)
	err := p.Apply(context.Background(), PluginRoute{
		PluginName: "llama-cpp",
		Version:    "0.1.0",
		Routes: []ProxyRoute{
			{LocationPath: "/plugins/llama-cpp/", UpstreamURL: "http://10.66.0.5:8080/"},
		},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	conf := filepath.Join(confDir, "llama-cpp.conf")
	body, err := os.ReadFile(conf)
	if err != nil {
		t.Fatalf("read conf: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "proxy_pass http://10.66.0.5:8080/;") {
		t.Errorf("conf missing proxy_pass: %s", got)
	}
	if !strings.Contains(got, "location ^~ /plugins/llama-cpp/") {
		t.Errorf("conf missing location prefix: %s", got)
	}
	if !strings.Contains(got, "X-Forwarded-Prefix /plugins/llama-cpp;") {
		t.Errorf("conf missing X-Forwarded-Prefix: %s", got)
	}
	if !strings.Contains(got, "Connection $connection_upgrade;") {
		t.Errorf("conf missing WebSocket upgrade: %s", got)
	}
	if rl.calls != 1 {
		t.Errorf("reload calls = %d want 1", rl.calls)
	}
}

func TestProxy_Apply_BaseConfWrittenOnFirstUse(t *testing.T) {
	p, _, _ := newTestProxy(t)
	if err := p.Apply(context.Background(), PluginRoute{
		PluginName: "x",
		Routes:     []ProxyRoute{{LocationPath: "/plugins/x/", UpstreamURL: "http://1.2.3.4:80/"}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	body, err := os.ReadFile(p.baseConfPath)
	if err != nil {
		t.Fatalf("base conf should be written: %v", err)
	}
	if !strings.Contains(string(body), "map $http_upgrade $connection_upgrade") {
		t.Errorf("base conf missing connection_upgrade map: %s", body)
	}
}

func TestProxy_Apply_MultiplePortsGetSuffixedPaths(t *testing.T) {
	p, _, confDir := newTestProxy(t)
	err := p.Apply(context.Background(), PluginRoute{
		PluginName: "myapp",
		Version:    "1.0.0",
		Routes: []ProxyRoute{
			{LocationPath: "/plugins/myapp/", UpstreamURL: "http://10.66.0.5:8080/"},
			{LocationPath: "/plugins/myapp/api/", UpstreamURL: "http://10.66.0.5:9090/"},
		},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(confDir, "myapp.conf"))
	if !strings.Contains(string(body), "location ^~ /plugins/myapp/") ||
		!strings.Contains(string(body), "location ^~ /plugins/myapp/api/") {
		t.Errorf("expected both locations in conf:\n%s", string(body))
	}
}

func TestProxy_Apply_BearerAuthInjectedWhenSet(t *testing.T) {
	p, _, confDir := newTestProxy(t)
	err := p.Apply(context.Background(), PluginRoute{
		PluginName: "x",
		Routes: []ProxyRoute{{
			LocationPath: "/plugins/x/",
			UpstreamURL:  "http://1.2.3.4:80/",
			AuthBearer:   "secret-token-abc",
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(confDir, "x.conf"))
	if !strings.Contains(string(body), `proxy_set_header Authorization "Bearer secret-token-abc";`) {
		t.Errorf("auth header missing:\n%s", body)
	}
}

func TestProxy_Apply_NoAuthDoesNotInjectHeader(t *testing.T) {
	p, _, confDir := newTestProxy(t)
	_ = p.Apply(context.Background(), PluginRoute{
		PluginName: "x",
		Routes:     []ProxyRoute{{LocationPath: "/plugins/x/", UpstreamURL: "http://1.2.3.4:80/"}},
	})
	body, _ := os.ReadFile(filepath.Join(confDir, "x.conf"))
	if strings.Contains(string(body), "Authorization") {
		t.Errorf("conf should not include Authorization when AuthBearer is empty:\n%s", body)
	}
}

func TestProxy_Apply_ReloadFailureRollsBackToPriorConf(t *testing.T) {
	p, rl, confDir := newTestProxy(t)
	// First apply succeeds.
	if err := p.Apply(context.Background(), PluginRoute{
		PluginName: "x",
		Routes:     []ProxyRoute{{LocationPath: "/plugins/x/", UpstreamURL: "http://1.2.3.4:80/"}},
	}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	priorBytes, _ := os.ReadFile(filepath.Join(confDir, "x.conf"))

	// Second apply: reload rejects the new conf.
	rl.failOnce = true
	err := p.Apply(context.Background(), PluginRoute{
		PluginName: "x",
		Routes:     []ProxyRoute{{LocationPath: "/plugins/x/", UpstreamURL: "http://9.9.9.9:80/"}},
	})
	if err == nil {
		t.Fatal("expected error from reload failure")
	}
	rolledBack, _ := os.ReadFile(filepath.Join(confDir, "x.conf"))
	if string(rolledBack) != string(priorBytes) {
		t.Errorf("conf should have rolled back to prior bytes")
	}
}

func TestProxy_Remove_DeletesConfAndReloads(t *testing.T) {
	p, rl, confDir := newTestProxy(t)
	if err := p.Apply(context.Background(), PluginRoute{
		PluginName: "x",
		Routes:     []ProxyRoute{{LocationPath: "/plugins/x/", UpstreamURL: "http://1.2.3.4:80/"}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rl.calls = 0

	if err := p.Remove(context.Background(), "x"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(confDir, "x.conf")); !os.IsNotExist(err) {
		t.Errorf("conf should be gone: %v", err)
	}
	if rl.calls != 1 {
		t.Errorf("reload calls = %d want 1", rl.calls)
	}
}

func TestProxy_Remove_MissingConfIsNoOp(t *testing.T) {
	p, rl, _ := newTestProxy(t)
	if err := p.Remove(context.Background(), "never-installed"); err != nil {
		t.Errorf("remove of missing conf should be a no-op: %v", err)
	}
	if rl.calls != 0 {
		t.Errorf("reload should not have been called: %d", rl.calls)
	}
}

func TestProxy_Apply_ZeroRoutesIsNoOp(t *testing.T) {
	p, rl, confDir := newTestProxy(t)
	if err := p.Apply(context.Background(), PluginRoute{PluginName: "x"}); err != nil {
		t.Errorf("zero routes should not error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(confDir, "x.conf")); !os.IsNotExist(err) {
		t.Errorf("no conf should be written for zero routes")
	}
	if rl.calls != 0 {
		t.Errorf("reload should not be called: %d", rl.calls)
	}
}
