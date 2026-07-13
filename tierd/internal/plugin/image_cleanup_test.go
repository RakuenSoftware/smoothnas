package plugin

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/compose"
	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

func TestNormalizeImageRef(t *testing.T) {
	cases := map[string]string{
		"nginx":                       "nginx:latest",
		"nginx:1.25":                  "nginx:1.25",
		"ghcr.io/org/app":             "ghcr.io/org/app:latest",
		"ghcr.io/org/app:testing":     "ghcr.io/org/app:testing",
		"registry:5000/app":           "registry:5000/app:latest",
		"registry:5000/app:v1":        "registry:5000/app:v1",
		"ghcr.io/org/app@sha256:dead": "ghcr.io/org/app@sha256:dead",
		"":                            "",
	}
	for in, want := range cases {
		if got := normalizeImageRef(in); got != want {
			t.Errorf("normalizeImageRef(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestPluginImageRefs_DedupesAcrossServicesAndRefs(t *testing.T) {
	rec := &PluginRecord{
		Services: []ServiceRow{
			{Service: "web", ImageRef: "nginx:1.25"},
			{Service: "db", ImageRef: "postgres:16"},
		},
		ContainerRefs: []ContainerRefRow{
			{Service: "web", ImageRef: "nginx:1.25", ResolvedRef: "nginx:1.25"},
			{Service: "db", ImageRef: "postgres:16", ResolvedRef: "postgres@sha256:abc"},
		},
	}
	got := pluginImageRefs(rec)
	sort.Strings(got)
	want := []string{"nginx:1.25", "postgres:16", "postgres@sha256:abc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pluginImageRefs=%v, want %v", got, want)
	}
}

// A compose plugin's uninstall must reclaim the images it pulled — `compose
// down` never removes them, which is the tier-storage leak this fixes.
func TestLifecycle_Demolish_Compose_RemovesImages(t *testing.T) {
	store := openTestStore(t)
	const project = "name: demo\nservices:\n  web:\n    image: ghcr.io/org/web:testing\n"
	if _, err := NewInstaller(store).Install([]byte(project)); err != nil {
		t.Fatalf("install: %v", err)
	}

	rt := &fakeRuntime{} // no managed containers -> nothing else references the image
	lc := NewLifecycle(store, rt)
	lc.SetComposeBackend(compose.NewBackend(compose.New("", &recRunner{}), t.TempDir()))

	if err := lc.Demolish(context.Background(), "demo"); err != nil {
		t.Fatalf("demolish: %v", err)
	}
	if want := []string{"ghcr.io/org/web:testing"}; !reflect.DeepEqual(rt.removeImages, want) {
		t.Fatalf("RemoveImage calls=%v, want %v", rt.removeImages, want)
	}
}

// Guard: an image another running plugin still references must NOT be removed
// when one plugin using it is uninstalled.
func TestLifecycle_Demolish_Compose_KeepsSharedImage(t *testing.T) {
	store := openTestStore(t)
	const project = "name: demo\nservices:\n  web:\n    image: shared:1\n"
	if _, err := NewInstaller(store).Install([]byte(project)); err != nil {
		t.Fatalf("install: %v", err)
	}

	rt := &fakeRuntime{
		managed: []runtime.ContainerSummary{{ID: "other", Image: "shared:1"}},
	}
	lc := NewLifecycle(store, rt)
	lc.SetComposeBackend(compose.NewBackend(compose.New("", &recRunner{}), t.TempDir()))

	if err := lc.Demolish(context.Background(), "demo"); err != nil {
		t.Fatalf("demolish: %v", err)
	}
	if len(rt.removeImages) != 0 {
		t.Fatalf("RemoveImage called %v, want none (image still in use)", rt.removeImages)
	}
}

func TestCleanupOrphanedImages(t *testing.T) {
	store := openTestStore(t)
	// An installed plugin tracks keep-me:latest, so it must survive the sweep
	// even though nothing is currently running.
	if _, err := NewInstaller(store).Install(
		[]byte("name: keeper\nservices:\n  web:\n    image: keep-me\n"),
	); err != nil {
		t.Fatalf("install: %v", err)
	}

	old := time.Now().Add(-2 * time.Hour).Unix()
	fresh := time.Now().Unix()
	rt := &fakeRuntime{
		images: []runtime.ImageSummary{
			{ID: "a", RepoTags: []string{"keep-me:latest"}, Containers: 0, Created: old},                                                // tracked -> keep
			{ID: "b", RepoTags: []string{"orphan:1.0"}, Containers: 0, Created: old},                                                    // orphan -> remove
			{ID: "c", RepoTags: []string{"inuse:1.0"}, Containers: 3, Created: old},                                                     // in use -> keep
			{ID: "d", RepoTags: []string{"fresh:1.0"}, Containers: 0, Created: fresh},                                                   // too new -> keep
			{ID: "e", RepoTags: []string{"<none>:<none>"}, RepoDigests: []string{"ghcr.io/x@sha256:dead"}, Containers: 0, Created: old}, // dangling orphan -> remove by digest
		},
	}
	lc := NewLifecycle(store, rt)

	removed, err := lc.CleanupOrphanedImages(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("CleanupOrphanedImages: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	sort.Strings(rt.removeImages)
	want := []string{"ghcr.io/x@sha256:dead", "orphan:1.0"}
	if !reflect.DeepEqual(rt.removeImages, want) {
		t.Fatalf("RemoveImage calls=%v, want %v", rt.removeImages, want)
	}
}

func TestStore_TrackedImageRefs(t *testing.T) {
	store := openTestStore(t)
	if _, err := NewInstaller(store).Install(
		[]byte("name: demo\nservices:\n  web:\n    image: ghcr.io/org/web:testing\n  db:\n    image: postgres:16\n"),
	); err != nil {
		t.Fatalf("install: %v", err)
	}
	refs, err := store.TrackedImageRefs()
	if err != nil {
		t.Fatalf("TrackedImageRefs: %v", err)
	}
	sort.Strings(refs)
	want := []string{"ghcr.io/org/web:testing", "postgres:16"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("TrackedImageRefs=%v, want %v", refs, want)
	}
}
