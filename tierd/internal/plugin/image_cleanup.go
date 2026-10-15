package plugin

import (
	"context"
	"errors"
	"fmt"
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

// pruneUnusedImages removes all aliases of an uninstalled plugin's images,
// while retaining images referenced by other installed plugins or containers.
func (l *Lifecycle) pruneUnusedImages(ctx context.Context, name string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	inUse, ok := l.imageRefsInUse(ctx)
	if !ok {
		return fmt.Errorf("query containers before image cleanup")
	}
	plugins, err := l.store.List()
	if err != nil {
		return err
	}
	for _, plugin := range plugins {
		if plugin.Name == name {
			continue
		}
		rec, err := l.store.Get(plugin.Name)
		if err != nil {
			return err
		}
		for _, ref := range pluginImageRefs(rec) {
			inUse[normalizeImageRef(ref)] = true
		}
	}
	images, err := l.rt.ListImages(ctx)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, ref := range refs {
		wanted[normalizeImageRef(ref)] = true
	}
	// The runtime untags one reference per deletion, including ID requests.
	// Reclaim every alias only after checking the complete shared-image inventory.
	removed := map[string]bool{}
	var errs []error
	for _, img := range coalesceImagesByID(images) {
		if !imageTracked(img, wanted) {
			continue
		}
		aliases := append(append([]string{}, img.RepoTags...), img.RepoDigests...)
		if img.Containers > 0 || imageTracked(img, inUse) || inUse[normalizeImageRef(img.ID)] {
			for _, ref := range aliases {
				inUse[normalizeImageRef(ref)] = true
			}
			continue
		}
		if err := l.removeImageTemplate(ctx, img); err != nil {
			errs = append(errs, err)
			continue
		}
		for _, alias := range aliases {
			removed[normalizeImageRef(alias)] = true
		}
	}
	for _, ref := range refs {
		n := normalizeImageRef(ref)
		if ref == "" || inUse[n] || removed[n] {
			continue
		}
		if err := l.rt.RemoveImage(ctx, ref); err != nil {
			errs = append(errs, err)
		}
		removed[n] = true
	}
	return errors.Join(errs...)
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
	for _, img := range coalesceImagesByID(images) {
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
		if err := l.removeImageTemplate(ctx, img); err != nil {
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
	if keep[normalizeImageRef(img.ID)] {
		return true
	}
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

// removeImageTemplate untags every known alias before deleting the final
// backing template. Verify inventory so an HTTP 200 untag is never mistaken
// for complete reclamation. Runtime conflicts and I/O errors remain visible.
func (l *Lifecycle) removeImageTemplate(ctx context.Context, img runtime.ImageSummary) error {
	refs := append(append(append([]string{}, img.RepoTags...), img.RepoDigests...), img.ID)
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || ref == "<none>:<none>" || seen[ref] {
			continue
		}
		seen[ref] = true
		if err := l.rt.RemoveImage(ctx, ref); err != nil {
			return err
		}
	}
	remaining, err := l.rt.ListImages(ctx)
	if err != nil {
		return err
	}
	for _, other := range remaining {
		if (img.ID != "" && other.ID == img.ID) || imageTracked(other, seen) {
			return fmt.Errorf("image %s still cached after removing aliases", img.ID)
		}
	}
	return nil
}

// The runtime can return a separate inventory row per alias of one template.
// Combine them before deciding whether that template is shared or removable.
func coalesceImagesByID(images []runtime.ImageSummary) []runtime.ImageSummary {
	var out []runtime.ImageSummary
	indices := map[string]int{}
	for _, img := range images {
		if index, ok := indices[img.ID]; ok && img.ID != "" {
			existing := &out[index]
			existing.RepoTags = append(existing.RepoTags, img.RepoTags...)
			existing.RepoDigests = append(existing.RepoDigests, img.RepoDigests...)
			if img.Containers > existing.Containers {
				existing.Containers = img.Containers
			}
			if img.Created > existing.Created {
				existing.Created = img.Created
			}
			continue
		}
		if img.ID != "" {
			indices[img.ID] = len(out)
		}
		img.RepoTags = append([]string{}, img.RepoTags...)
		img.RepoDigests = append([]string{}, img.RepoDigests...)
		out = append(out, img)
	}
	return out
}
