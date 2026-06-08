package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

// HostsResyncInterval is how often the reconciler rewrites every
// managed container's /etc/hosts so a sibling that drifted to a new
// bridge IP is picked up without recreating the dependent. Short
// enough that a dependent's reconnect after a backend recreate
// succeeds within seconds, cheap enough to run forever (a handful of
// inspects + writes only when content actually changed).
const HostsResyncInterval = 15 * time.Second

// hostEntry is one name→IP record in a generated /etc/hosts.
type hostEntry struct {
	name string
	ip   string
}

// hostsRuntime is the slice of the runtime client the hosts sweep
// needs. Both *Lifecycle's and *Reconciler's RuntimeClient satisfy
// it, so no new interface methods (or test fakes) are required.
type hostsRuntime interface {
	ListManagedContainers(ctx context.Context) ([]runtime.ContainerSummary, error)
	InspectContainer(ctx context.Context, id string) (runtime.ContainerInspect, error)
}

// renderEtcHosts builds an /etc/hosts file body mapping each peer
// container name to its current bridge IP. Pure and deterministic
// (entries are sorted) so a re-render with unchanged inputs produces
// byte-identical output and writeHostsFile can skip the write.
func renderEtcHosts(entries []hostEntry) string {
	var b strings.Builder
	b.WriteString("# Managed by SmoothNAS tierd — regenerated on every reconcile.\n")
	b.WriteString("# LXC2Docker has no embedded DNS; this lets plugin containers\n")
	b.WriteString("# reach same-plugin siblings by stable name across bridge IP drift.\n")
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")

	seen := map[string]bool{}
	uniq := make([]hostEntry, 0, len(entries))
	for _, e := range entries {
		if e.name == "" || e.ip == "" || seen[e.name] {
			continue
		}
		seen[e.name] = true
		uniq = append(uniq, e)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].name < uniq[j].name })
	for _, e := range uniq {
		fmt.Fprintf(&b, "%s\t%s\n", e.ip, e.name)
	}
	return b.String()
}

// containerHostsPath derives the host-side path of a container's
// /etc/hosts from its inspect data. LXC2Docker reports HostnamePath
// (…/rootfs/etc/hostname) and leaves /etc/hosts as a plain rootfs
// file, so its sibling in the same directory is the one the container
// actually reads. Returns "" when the daemon didn't report a path.
func containerHostsPath(d runtime.ContainerInspect) string {
	if d.HostnamePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.HostnamePath), "hosts")
}

// writeHostsFile atomically writes content to path, but only when it
// differs from what's already there. Ownership and mode are copied
// from ownerRef (the container's /etc/hostname, which always exists
// and carries the correct, possibly uid-shifted, ownership) so a
// user-namespaced container can still read the file.
func writeHostsFile(path, ownerRef, content string) error {
	if path == "" {
		return errors.New("empty hosts path")
	}
	if cur, err := os.ReadFile(path); err == nil && string(cur) == content {
		return nil // unchanged — skip the write
	}

	uid, gid := 0, 0
	if fi, err := os.Stat(ownerRef); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hosts-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, 0o644)
	_ = os.Chown(tmpName, uid, gid)
	return os.Rename(tmpName, path)
}

// containerInfo is the per-container data a hosts sweep needs.
type containerInfo struct {
	name      string
	plugin    string
	ip        string
	hostsPath string
	ownerRef  string
}

// collectManagedHosts inspects every managed container and returns
// them grouped by plugin. Containers that vanish between the list and
// the inspect, or that carry no plugin label, are skipped.
func collectManagedHosts(ctx context.Context, rt hostsRuntime) (map[string][]containerInfo, error) {
	managed, err := rt.ListManagedContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	byPlugin := map[string][]containerInfo{}
	for _, c := range managed {
		d, err := rt.InspectContainer(ctx, c.ID)
		if err != nil {
			continue // gone or unreadable; the next sweep will retry
		}
		plugin := c.Labels[runtime.PluginNameLabel]
		if plugin == "" {
			plugin = d.Config.Labels[runtime.PluginNameLabel]
		}
		if plugin == "" {
			continue
		}
		byPlugin[plugin] = append(byPlugin[plugin], containerInfo{
			name:      containerDisplayName(c, d),
			plugin:    plugin,
			ip:        pickBridgeIP(d),
			hostsPath: containerHostsPath(d),
			ownerRef:  d.HostnamePath,
		})
	}
	return byPlugin, nil
}

// syncManagedHosts rewrites /etc/hosts in every managed container so
// each can resolve its same-plugin siblings by name at the current
// bridge IP. Idempotent and safe to call on a ticker: only files
// whose content changed are rewritten. Per-container failures are
// collected and returned joined; they never abort the sweep.
func syncManagedHosts(ctx context.Context, rt hostsRuntime) error {
	byPlugin, err := collectManagedHosts(ctx, rt)
	if err != nil {
		return err
	}
	var errs []error
	for _, infos := range byPlugin {
		entries := make([]hostEntry, 0, len(infos))
		for _, in := range infos {
			entries = append(entries, hostEntry{name: in.name, ip: in.ip})
		}
		content := renderEtcHosts(entries)
		for _, in := range infos {
			if in.hostsPath == "" {
				continue // daemon reported no rootfs path (e.g. not yet started)
			}
			if err := writeHostsFile(in.hostsPath, in.ownerRef, content); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", in.name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// containerDisplayName is the bridge hostname for a container — its
// Docker name with the leading "/" stripped. This is the name tierd
// renders into discovery tokens, so /etc/hosts must key on the same.
func containerDisplayName(c runtime.ContainerSummary, d runtime.ContainerInspect) string {
	if len(c.Names) > 0 && c.Names[0] != "" {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return strings.TrimPrefix(d.Name, "/")
}

// pluginHostEntries builds the name→IP records for a single plugin
// from its recorded instance-1 bridge IPs. Used to seed a dependent's
// /etc/hosts at start time (the daemon-wide sweep in syncManagedHosts
// reads live IPs instead). Services without a recorded IP yet are
// skipped — they contribute nothing the caller could resolve.
func pluginHostEntries(rec *PluginRecord, count int) []hostEntry {
	out := make([]hostEntry, 0, len(rec.Services))
	for _, sr := range rec.Services {
		for _, in := range rec.Instances {
			if in.Service == sr.Service && in.Instance == 1 && in.BridgeIP != "" {
				out = append(out, hostEntry{
					name: ContainerName(rec.Plugin.Name, sr.Service, 1, count),
					ip:   in.BridgeIP,
				})
			}
		}
	}
	return out
}
