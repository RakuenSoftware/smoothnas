package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/plugin/runtime"
)

var lxcContainerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

type runtimeContainerLister interface {
	ListContainers(ctx context.Context) ([]runtime.ContainerSummary, error)
}

func (l *Lifecycle) removeContainerWithCleanup(ctx context.Context, id string, force bool) error {
	if err := l.rt.RemoveContainer(ctx, id, force); err != nil {
		return err
	}
	return l.removeLXCContainerDir(id)
}

func (l *Lifecycle) removeLXCContainerDir(id string) error {
	dir, ok := l.lxcContainerDir(id)
	if !ok {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove lxc container dir %s: %w", dir, err)
	}
	return nil
}

// CleanupOrphanedLXCDirs removes stale smoothnas-runtime LXC backing
// directories that no longer correspond to any container known by the runtime
// daemon. minAge protects against racing a container that was created after the
// runtime list was captured.
func (l *Lifecycle) CleanupOrphanedLXCDirs(ctx context.Context, minAge time.Duration) (int, error) {
	root := strings.TrimSpace(l.lxcPath)
	if root == "" {
		return 0, nil
	}
	lister, ok := l.rt.(runtimeContainerLister)
	if !ok {
		return 0, nil
	}

	containers, err := lister.ListContainers(ctx)
	if err != nil {
		return 0, fmt.Errorf("list runtime containers: %w", err)
	}
	live := map[string]struct{}{}
	for _, c := range containers {
		addContainerKey(live, c.ID)
		for _, name := range c.Names {
			addContainerKey(live, strings.TrimPrefix(name, "/"))
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	cutoff := time.Now().Add(-minAge)
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !entry.IsDir() || !lxcContainerIDPattern.MatchString(entry.Name()) {
			continue
		}
		if _, ok := live[entry.Name()]; ok {
			continue
		}
		dir, ok := l.lxcContainerDir(entry.Name())
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		if minAge > 0 && info.ModTime().After(cutoff) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "config")); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		if err := os.RemoveAll(dir); err != nil {
			return removed, fmt.Errorf("remove orphaned lxc container dir %s: %w", dir, err)
		}
		removed++
	}
	return removed, nil
}

func (l *Lifecycle) lxcContainerDir(id string) (string, bool) {
	root := strings.TrimSpace(l.lxcPath)
	id = strings.TrimSpace(id)
	if root == "" || id == "" || filepath.Base(id) != id {
		return "", false
	}
	root = filepath.Clean(root)
	dir := filepath.Join(root, id)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", false
	}
	return dir, true
}

func addContainerKey(keys map[string]struct{}, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	keys[value] = struct{}{}
	if len(value) >= 12 {
		keys[value[:12]] = struct{}{}
	}
}
