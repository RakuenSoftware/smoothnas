package plugin

import (
	"context"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

// normalizeImageRef mirrors the runtime daemon: a bare name gets the implicit
// ":latest" tag so tag/ID comparisons line up with what the daemon reports.
// Digest-pinned refs (containing '@') are left whole.
func normalizeImageRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.Contains(ref, "@") {
		return ref
	}
	// Only the last colon after the final slash is a tag separator (a colon
	// before a slash is a registry port).
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref
	}
	return ref + ":latest"
}

// pluginImageRefs returns the deduplicated set of image refs a plugin record
// tracks: every container-ref's resolved/mutable ref plus each service's
// recorded image. This is what a plugin "brought in" and should take away on
// uninstall.
func pluginImageRefs(rec *PluginRecord) []string {
	if rec == nil {
		return nil
	}
	seen := map[string]bool{}
	var refs []string
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	for _, cr := range rec.ContainerRefs {
		add(cr.ResolvedRef)
		add(cr.ImageRef)
	}
	for _, sr := range rec.Services {
		add(sr.ImageRef)
	}
	return refs
}

// imageRefsInUse returns the set of normalized image refs referenced by a live
// managed container. Returns (nil, false) if the runtime can't be queried, so
// callers can choose to stay conservative rather than remove a shared image.
func (l *Lifecycle) imageRefsInUse(ctx context.Context) (map[string]bool, bool) {
	managed, err := l.rt.ListManagedContainers(ctx)
	if err != nil {
		return nil, false
	}
	inUse := make(map[string]bool, len(managed))
	for _, c := range managed {
		if ref := normalizeImageRef(c.Image); ref != "" {
			inUse[ref] = true
		}
	}
	return inUse, true
}

// pruneUnusedImages removes each ref that no live managed container still
// references. Best-effort: a RemoveImage failure is swallowed (idempotent on
// 404), and if the in-use set can't be computed we skip removal entirely rather
// than risk deleting an image another running plugin depends on.
func (l *Lifecycle) pruneUnusedImages(ctx context.Context, refs []string) {
	if len(refs) == 0 {
		return
	}
	inUse, ok := l.imageRefsInUse(ctx)
	if !ok {
		return
	}
	for _, ref := range refs {
		if ref == "" || inUse[normalizeImageRef(ref)] {
			continue
		}
		_ = l.rt.RemoveImage(ctx, ref)
	}
}

// CleanupOrphanedImages reclaims runtime image templates that no longer belong
// to anything: no container references them (the daemon's own usage count is 0)
// and no installed plugin tracks them. This catches images left behind by an
// in-place image update (compose pull of a new digest orphans the old template)
// and any earlier uninstall that predates image reclamation.
//
// minAge protects a freshly pulled image that hasn't been wired to a container
// yet (Materialise pulls before Start creates the container), matching
// CleanupOrphanedLXCDirs. Returns the number of images removed.
func (l *Lifecycle) CleanupOrphanedImages(ctx context.Context, minAge time.Duration) (int, error) {
	images, err := l.rt.ListImages(ctx)
	if err != nil {
		return 0, err
	}
	tracked, err := l.store.TrackedImageRefs()
	if err != nil {
		return 0, err
	}
	keep := make(map[string]bool, len(tracked))
	for _, ref := range tracked {
		if n := normalizeImageRef(ref); n != "" {
			keep[n] = true
		}
	}

	cutoff := time.Now().Add(-minAge)
	removed := 0
	for _, img := range images {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		// A container (managed or not) still uses it — leave it alone.
		if img.Containers > 0 {
			continue
		}
		// An installed plugin still tracks it (e.g. stopped plugin with no
		// live containers) — not garbage.
		if imageTracked(img, keep) {
			continue
		}
		if minAge > 0 && img.Created > 0 && time.Unix(img.Created, 0).After(cutoff) {
			continue
		}
		ref := removalRef(img)
		if ref == "" {
			continue
		}
		if err := l.rt.RemoveImage(ctx, ref); err != nil {
			// Best-effort sweep: log-and-continue is the caller's job; one
			// stuck image shouldn't abort the rest.
			continue
		}
		removed++
	}
	return removed, nil
}

// imageTracked reports whether any of an image's tags/digests is in keep.
func imageTracked(img runtime.ImageSummary, keep map[string]bool) bool {
	for _, t := range img.RepoTags {
		if keep[normalizeImageRef(t)] {
			return true
		}
	}
	for _, d := range img.RepoDigests {
		if keep[normalizeImageRef(d)] {
			return true
		}
	}
	return false
}

// removalRef picks the ref to pass to RemoveImage: a real tag when present,
// else a digest ref, else the image ID.
func removalRef(img runtime.ImageSummary) string {
	for _, t := range img.RepoTags {
		if t = strings.TrimSpace(t); t != "" && t != "<none>:<none>" {
			return t
		}
	}
	for _, d := range img.RepoDigests {
		if d = strings.TrimSpace(d); d != "" {
			return d
		}
	}
	return strings.TrimSpace(img.ID)
}
