package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// DefaultNginxConfDir is the parent directory where tierd writes
// per-plugin nginx routes. The parent SmoothNAS nginx config must
// `include /etc/nginx/conf.d/plugins.d/*.conf;` for these files to
// take effect — that include is added by the iso/hooks installer
// (separate change, tracked in the phase 04 PR description).
const DefaultNginxConfDir = "/etc/nginx/conf.d/plugins.d"

// DefaultNginxBaseConfPath is the one-time include that defines
// shared bits (the connection_upgrade map for WebSocket support)
// every per-plugin route relies on. Written once on first plugin
// install; safe to overwrite if absent.
const DefaultNginxBaseConfPath = "/etc/nginx/conf.d/smoothnas-plugins-base.conf"

// NginxReloader runs `nginx -t && systemctl reload nginx`. Injected
// into Proxy so tests can supply a recording fake. Production uses
// SystemdNginxReloader.
type NginxReloader interface {
	// Reload validates the current nginx config tree and, on success,
	// reloads the running nginx. On test failure, returns an error
	// containing the nginx -t output so the operator (or test) can
	// inspect what went wrong.
	Reload(ctx context.Context) error
}

// SystemdNginxReloader shells out to the host's nginx + systemctl.
// The default in production. Tests inject a fake.
type SystemdNginxReloader struct{}

// Reload runs `nginx -t` then `systemctl reload nginx` on success.
func (SystemdNginxReloader) Reload(ctx context.Context) error {
	test := exec.CommandContext(ctx, "nginx", "-t")
	out, err := test.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nginx -t failed: %w\n%s", err, out)
	}
	reload := exec.CommandContext(ctx, "systemctl", "reload", "nginx")
	if out, err := reload.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl reload nginx: %w\n%s", err, out)
	}
	return nil
}

// Proxy owns the on-disk nginx config tree for plugin routes.
// One Proxy is shared across the lifecycle, and writes config files
// + invokes nginx reload through the configured NginxReloader.
type Proxy struct {
	confDir      string
	baseConfPath string
	reloader     NginxReloader

	// Overridable for tests so we don't actually touch the
	// filesystem. Default to os.WriteFile / os.MkdirAll / os.Remove.
	writeFile func(name string, data []byte, perm os.FileMode) error
	mkdirAll  func(path string, perm os.FileMode) error
	remove    func(path string) error
}

// NewProxy constructs a Proxy with production defaults. Override
// via SetConfDir / SetReloader / SetWriteFile etc. for tests.
func NewProxy() *Proxy {
	return &Proxy{
		confDir:      DefaultNginxConfDir,
		baseConfPath: DefaultNginxBaseConfPath,
		reloader:     SystemdNginxReloader{},
		writeFile:    os.WriteFile,
		mkdirAll:     os.MkdirAll,
		remove:       os.Remove,
	}
}

// SetConfDir overrides the parent directory for per-plugin routes.
// Used by tests; production callers stick with DefaultNginxConfDir.
func (p *Proxy) SetConfDir(dir string)             { p.confDir = dir }
func (p *Proxy) SetBaseConfPath(path string)       { p.baseConfPath = path }
func (p *Proxy) SetReloader(r NginxReloader)       { p.reloader = r }
func (p *Proxy) SetWriteFileFn(fn func(string, []byte, os.FileMode) error) {
	p.writeFile = fn
}
func (p *Proxy) SetMkdirAllFn(fn func(string, os.FileMode) error) { p.mkdirAll = fn }
func (p *Proxy) SetRemoveFn(fn func(string) error)                { p.remove = fn }

// PluginRoute is the per-plugin input to the nginx template.
// Lifecycle builds one of these from a PluginRecord + per-instance
// bridge IP and passes it to Proxy.Apply.
type PluginRoute struct {
	PluginName string
	Version    string
	// Routes is one entry per nginx location. The first entry maps
	// to /plugins/<name>/; subsequent entries get
	// /plugins/<name>/<port-name>/. Entries are produced from the
	// manifest's Ports declaration order.
	Routes []ProxyRoute
}

// ProxyRoute is one nginx location → upstream mapping.
type ProxyRoute struct {
	// LocationPath is the nginx location prefix, e.g.
	// "/plugins/llama-cpp/" or "/plugins/llama-cpp/api/".
	LocationPath string
	// UpstreamURL is the http://<bridge-ip>:<port>/ form.
	UpstreamURL string
	// AuthBearer, when non-empty, is injected as
	// `proxy_set_header Authorization "Bearer <token>";`. Phase 07
	// fills this in for plugins with ui.embed.auth=bearer-injected.
	AuthBearer string
}

// Apply renders, writes, and reloads. Returns an error if any step
// fails; on reload-failure (nginx -t rejects the new conf) the new
// file is removed and reload is retried so the previous valid conf
// stays active.
func (p *Proxy) Apply(ctx context.Context, route PluginRoute) error {
	if route.PluginName == "" {
		return fmt.Errorf("Apply: empty plugin name")
	}
	if len(route.Routes) == 0 {
		// No exposed ports — nothing to do. Caller should still call
		// Remove on uninstall to clean up any prior conf.
		return nil
	}

	if err := p.mkdirAll(p.confDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", p.confDir, err)
	}
	if err := p.ensureBaseConf(); err != nil {
		return err
	}

	body, err := renderProxyConfig(route)
	if err != nil {
		return fmt.Errorf("render conf for %s: %w", route.PluginName, err)
	}
	confPath := p.confPathFor(route.PluginName)

	// Snapshot any existing file so we can roll back on reload
	// failure. nginx -t rejects bad config without us having to
	// detect it; we just need to put the old bytes back.
	prior, hadPrior := p.snapshot(confPath)

	if err := p.writeFile(confPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", confPath, err)
	}
	if err := p.reloader.Reload(ctx); err != nil {
		// Roll back to whatever was there before.
		if hadPrior {
			_ = p.writeFile(confPath, prior, 0o644)
		} else {
			_ = p.remove(confPath)
		}
		// Best-effort second reload to surface reverted state.
		_ = p.reloader.Reload(ctx)
		return fmt.Errorf("nginx reload rejected new conf for %s: %w", route.PluginName, err)
	}
	return nil
}

// Remove deletes the per-plugin nginx conf and reloads. Idempotent —
// calling Remove for a plugin that has no conf is fine.
func (p *Proxy) Remove(ctx context.Context, pluginName string) error {
	confPath := p.confPathFor(pluginName)
	if _, err := os.Stat(confPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := p.remove(confPath); err != nil {
		return fmt.Errorf("remove %s: %w", confPath, err)
	}
	if err := p.reloader.Reload(ctx); err != nil {
		return fmt.Errorf("nginx reload after %s removal: %w", pluginName, err)
	}
	return nil
}

// confPathFor returns the per-plugin conf file path.
func (p *Proxy) confPathFor(pluginName string) string {
	return filepath.Join(p.confDir, pluginName+".conf")
}

// snapshot reads the prior conf file bytes (if any) so reload
// rollback can restore them. Returns (data, true) when the file
// existed, ([]byte{}, false) otherwise.
func (p *Proxy) snapshot(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

// ensureBaseConf writes the connection_upgrade map (and any other
// once-only directives the per-plugin routes depend on) if it is
// not already present. Idempotent: if the file exists with content
// we recognise, leave it alone.
func (p *Proxy) ensureBaseConf() error {
	if _, err := os.Stat(p.baseConfPath); err == nil {
		return nil
	}
	if err := p.mkdirAll(filepath.Dir(p.baseConfPath), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(p.baseConfPath), err)
	}
	if err := p.writeFile(p.baseConfPath, []byte(baseConfBody), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", p.baseConfPath, err)
	}
	return nil
}

// baseConfBody is the once-per-host include that defines variables
// shared across every per-plugin route. Lives in
// /etc/nginx/conf.d/smoothnas-plugins-base.conf.
const baseConfBody = `# Generated by tierd. Do not edit; regenerated on first plugin install.
# Defines shared directives the per-plugin /etc/nginx/conf.d/plugins.d/*.conf
# routes depend on. Currently just the WebSocket connection-upgrade map.

map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}
`

// renderProxyConfig produces the per-plugin nginx config from a
// PluginRoute. Each PluginRoute.Routes entry becomes one location {}.
func renderProxyConfig(route PluginRoute) (string, error) {
	var sb strings.Builder
	if err := proxyTmpl.Execute(&sb, route); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// proxyTmpl is parsed once at init so per-call rendering doesn't
// re-parse. trimSlash is registered via the FuncMap so the template
// body can call it.
var proxyTmpl = template.Must(template.New("plugin-route").
	Funcs(template.FuncMap{"trimSlash": trimSlash}).
	Parse(proxyConfTemplate))

// proxyConfTemplate is the per-plugin nginx fragment. Streaming-
// friendly defaults and WebSocket support are baked in because
// most plugin UIs (llama.cpp, jellyfin, code-server) need them.
const proxyConfTemplate = `# Generated by tierd; do not edit. Plugin: {{.PluginName}} v{{.Version}}
{{range .Routes}}
location ^~ {{.LocationPath}} {
    proxy_pass {{.UpstreamURL}};
    proxy_http_version 1.1;
    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Prefix {{trimSlash .LocationPath}};
{{- if .AuthBearer }}
    proxy_set_header Authorization "Bearer {{.AuthBearer}}";
{{- end }}

    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection $connection_upgrade;

    proxy_buffering          off;
    proxy_request_buffering  off;
    proxy_read_timeout       1d;
    proxy_send_timeout       1d;
}
{{end}}`

// trimSlash returns path with any trailing slash removed. Used by
// the template so X-Forwarded-Prefix is "/plugins/foo" rather than
// "/plugins/foo/" (matches nginx convention for the header).
func trimSlash(path string) string {
	return strings.TrimRight(path, "/")
}
