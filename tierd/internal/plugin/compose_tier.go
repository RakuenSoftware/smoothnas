package plugin

import (
	"fmt"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
)

// ResolveComposeTierVolumes resolves each x-smoothnas tiered volume of a compose
// plugin to a smoothfs host path (reusing the tier provider + the same
// ResolveTierBoundHostPath layout as manifest plugins). Returns volume-name ->
// host-path, or an error if a tier is missing/unhealthy. Callers bind these into
// the compose via compose.RewriteTieredBinds. The path is deterministic; callers
// PIN it so an operator retier cannot silently relocate live data.
func ResolveComposeTierVolumes(tp TierProvider, pluginName string, tvols []compose.TieredVolume) (map[string]string, error) {
	if len(tvols) == 0 {
		return nil, nil
	}
	if tp == nil {
		return nil, fmt.Errorf("compose plugin %q declares tiered volumes but no tier provider is wired", pluginName)
	}
	binds := map[string]string{}
	for _, tv := range tvols {
		pool, err := tp.GetTierInstance(tv.Tier)
		if err != nil {
			return nil, fmt.Errorf("tiered volume %q: tier %q: %w", tv.Name, tv.Tier, err)
		}
		if !poolReady(pool) {
			return nil, fmt.Errorf("tiered volume %q: tier %q is in state %q (need healthy or degraded)", tv.Name, tv.Tier, pool.State)
		}
		binds[tv.Name] = ResolveTierBoundHostPath(pool.MountPoint, pluginName, "compose", tv.Name)
	}
	return binds, nil
}
