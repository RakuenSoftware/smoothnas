package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

const (
	defaultRuntimeLXCPath   = "/var/lib/smoothnas/runtime/lxc"
	defaultRuntimeStatePath = "/var/lib/smoothnas/runtime/state"
	runtimeEnvPath          = "/etc/smoothnas/runtime.env"
	runtimeTierSubdir       = ".smoothnas/runtime"
)

func configureRuntimeStorage() {
	dbPath := os.Getenv("TIERD_DB")
	if dbPath == "" {
		dbPath = "/var/lib/tierd/tierd.db"
	}
	store, err := db.Open(dbPath)
	if err != nil {
		log.Printf("runtime storage: open db: %v", err)
		return
	}
	defer store.Close()

	pools, err := store.ListSmoothfsPools()
	if err != nil {
		log.Printf("runtime storage: list smoothfs pools: %v", err)
		return
	}
	root, ok := runtimeRootFromSmoothfsPools(pools)
	if !ok {
		log.Printf("runtime storage: no smoothfs tier found, using %s", defaultRuntimeLXCPath)
		return
	}
	lxcPath, statePath, err := runtimePathsFromRoot(root)
	if err != nil {
		log.Printf("runtime storage: %v", err)
		return
	}
	if err := migrateRuntimeStorage(defaultRuntimeLXCPath, lxcPath); err != nil {
		log.Printf("runtime storage: migrate lxc: %v", err)
	}
	if err := rewriteLXCConfigPaths(defaultRuntimeLXCPath, lxcPath); err != nil {
		log.Printf("runtime storage: rewrite lxc config paths: %v", err)
	}
	if err := migrateRuntimeStorage(defaultRuntimeStatePath, statePath); err != nil {
		log.Printf("runtime storage: migrate state: %v", err)
	}
	for _, path := range []string{lxcPath, statePath, filepath.Dir(runtimeEnvPath)} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			log.Printf("runtime storage: mkdir %s: %v", path, err)
			return
		}
	}
	data := "SMOOTHNAS_RUNTIME_LXCPATH=" + strconv.Quote(lxcPath) + "\n" +
		"SMOOTHNAS_RUNTIME_STATEPATH=" + strconv.Quote(statePath) + "\n"
	if err := os.WriteFile(runtimeEnvPath, []byte(data), 0o644); err != nil {
		log.Printf("runtime storage: write %s: %v", runtimeEnvPath, err)
		return
	}
	log.Printf("runtime storage: configured lxc=%s state=%s", lxcPath, statePath)
}

func runtimeRootFromSmoothfsPools(pools []db.SmoothfsPool) (string, bool) {
	pool, ok := preferredRuntimePool(pools)
	if !ok {
		return "", false
	}
	for _, tierPath := range pool.Tiers {
		tierPath = strings.TrimSpace(tierPath)
		if tierPath == "" {
			continue
		}
		return filepath.Join(tierPath, runtimeTierSubdir), true
	}
	return "", false
}

func preferredRuntimePool(pools []db.SmoothfsPool) (db.SmoothfsPool, bool) {
	for _, pool := range pools {
		if pool.Name == "media" && len(pool.Tiers) > 0 {
			return pool, true
		}
	}
	for _, pool := range pools {
		if len(pool.Tiers) > 0 {
			return pool, true
		}
	}
	return db.SmoothfsPool{}, false
}

func runtimePathsFromRoot(root string) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", fmt.Errorf("runtime root is empty")
	}
	return filepath.Join(root, "lxc"), filepath.Join(root, "state"), nil
}

func lxcPathFromConfig() string {
	if value := strings.TrimSpace(os.Getenv("SMOOTHNAS_RUNTIME_LXCPATH")); value != "" {
		return value
	}
	data, err := os.ReadFile(runtimeEnvPath)
	if err != nil {
		return defaultRuntimeLXCPath
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "SMOOTHNAS_RUNTIME_LXCPATH" {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		if value != "" {
			return value
		}
	}
	return defaultRuntimeLXCPath
}

func migrateRuntimeStorage(oldPath, newPath string) error {
	if oldPath == "" || newPath == "" || filepath.Clean(oldPath) == filepath.Clean(newPath) {
		return nil
	}
	if _, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	marker := filepath.Join(newPath, ".migrated-from-var-lib")
	if _, err := os.Stat(marker); err == nil {
		return os.RemoveAll(oldPath)
	}
	if hasEntries, err := dirHasEntries(newPath); err != nil {
		return err
	} else if hasEntries {
		return fmt.Errorf("target %s is not empty and has no migration marker", newPath)
	}
	if err := os.MkdirAll(newPath, 0o755); err != nil {
		return err
	}
	src := filepath.Clean(oldPath) + string(os.PathSeparator)
	dst := filepath.Clean(newPath) + string(os.PathSeparator)
	cmd := exec.Command("rsync", "-aHAX", "--numeric-ids", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync %s to %s: %s: %w", oldPath, newPath, strings.TrimSpace(string(out)), err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return err
	}
	return os.RemoveAll(oldPath)
}

func dirHasEntries(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return len(entries) > 0, nil
}

func rewriteLXCConfigPaths(oldPath, newPath string) error {
	if oldPath == "" || newPath == "" || filepath.Clean(oldPath) == filepath.Clean(newPath) {
		return nil
	}
	return filepath.WalkDir(newPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "config" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		oldText := string(data)
		newText := strings.ReplaceAll(oldText, filepath.Clean(oldPath)+string(os.PathSeparator), filepath.Clean(newPath)+string(os.PathSeparator))
		if newText == oldText {
			return nil
		}
		return os.WriteFile(path, []byte(newText), 0o644)
	})
}
