package nonraid

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

var runtimeCommand = exec.Command

type RunningArray struct {
	engine  *Engine
	servers []*NBDServer
	files   []*os.File
}

// Runtime owns live nonRaid NBD devices for the current tierd process.
type Runtime struct {
	mu     sync.Mutex
	arrays map[string]*RunningArray
}

func NewRuntime() *Runtime {
	return &Runtime{arrays: make(map[string]*RunningArray)}
}

func (rt *Runtime) Activate(r *http.Request, store *db.Store, row *db.NonRaidArrayRow, destructive bool) error {
	rt.mu.Lock()
	if _, ok := rt.arrays[row.Name]; ok {
		rt.mu.Unlock()
		return nil
	}
	rt.mu.Unlock()

	dataRows, parityRows := splitNonRaidDevices(row.Devices)
	if len(dataRows) == 0 {
		return fmt.Errorf("nonRaid array %s has no data devices", row.Name)
	}
	if len(parityRows) == 0 || len(parityRows) > MaxParityDevices {
		return fmt.Errorf("nonRaid array %s has %d parity devices, want 1..%d", row.Name, len(parityRows), MaxParityDevices)
	}

	if err := runRuntimeCommand("modprobe", "nbd", "nbds_max=64", "max_part=0"); err != nil {
		return err
	}
	if err := ensureRuntimeModule("smoothfs"); err != nil {
		return err
	}
	if destructive {
		for _, dev := range append(append([]db.NonRaidDeviceRow{}, dataRows...), parityRows...) {
			if err := runRuntimeCommand("wipefs", "-a", dev.DevicePath); err != nil {
				return err
			}
		}
	}

	running := &RunningArray{}
	defer func() {
		if running != nil {
			running.close()
		}
	}()

	dataDevs := make([]BlockDevice, 0, len(dataRows))
	dataSizes := make([]int64, 0, len(dataRows))
	for _, dev := range dataRows {
		f, err := os.OpenFile(dev.DevicePath, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open data disk %s: %w", dev.DevicePath, err)
		}
		running.files = append(running.files, f)
		dataDevs = append(dataDevs, f)
		dataSizes = append(dataSizes, int64(dev.UsableBytes))
	}
	parityDevs := make([]BlockDevice, 0, len(parityRows))
	for _, dev := range parityRows {
		f, err := os.OpenFile(dev.DevicePath, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open parity disk %s: %w", dev.DevicePath, err)
		}
		running.files = append(running.files, f)
		parityDevs = append(parityDevs, f)
	}

	engine, err := NewEngine(dataDevs, dataSizes, parityDevs, int64(row.MinParityBytes))
	if err != nil {
		return err
	}
	running.engine = engine
	if destructive {
		if err := engine.BuildParity(16 << 20); err != nil {
			return fmt.Errorf("initial parity sync: %w", err)
		}
	}

	for i, dev := range dataRows {
		nbdPath, err := findFreeNBD()
		if err != nil {
			return err
		}
		server, err := StartNBD(r.Context(), nbdPath, engine, i)
		if err != nil {
			return err
		}
		running.servers = append(running.servers, server)
		if err := store.SetNonRaidDeviceRuntime(row.ID, RoleData, dev.Slot, nbdPath, StateActive); err != nil {
			return err
		}
		if destructive || !blockDeviceHasFilesystem(nbdPath) {
			if err := makeFilesystem(row.Filesystem, nbdPath); err != nil {
				return err
			}
		}
		if err := mountDevice(nbdPath, dev.MountPath); err != nil {
			return err
		}
	}
	for _, dev := range parityRows {
		if err := store.SetNonRaidDeviceRuntime(row.ID, RoleParity, dev.Slot, "", StateActive); err != nil {
			return err
		}
	}
	if err := mountSmoothfs(row); err != nil {
		return err
	}
	if err := store.SetNonRaidArrayState(row.Name, StateActive, ""); err != nil {
		return err
	}

	rt.mu.Lock()
	rt.arrays[row.Name] = running
	rt.mu.Unlock()
	running = nil
	return nil
}

func (rt *Runtime) Deactivate(r *http.Request, store *db.Store, row *db.NonRaidArrayRow) error {
	rt.mu.Lock()
	running := rt.arrays[row.Name]
	delete(rt.arrays, row.Name)
	rt.mu.Unlock()

	var firstErr error
	setErr := func(err error) {
		if firstErr == nil && err != nil {
			firstErr = err
		}
	}

	if isMountpoint(row.MountPath) {
		setErr(unmount(row.MountPath))
	}
	dataRows, _ := splitNonRaidDevices(row.Devices)
	for _, dev := range dataRows {
		if dev.MountPath != "" && isMountpoint(dev.MountPath) {
			setErr(unmount(dev.MountPath))
		}
	}
	if running != nil {
		setErr(running.close())
	} else {
		for _, dev := range dataRows {
			if dev.VirtualDevicePath == "" {
				continue
			}
			setErr(disconnectNBD(dev.VirtualDevicePath))
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return store.SetNonRaidArrayState(row.Name, StateConfigured, "")
}

func (ra *RunningArray) close() error {
	var firstErr error
	for i := len(ra.servers) - 1; i >= 0; i-- {
		if err := ra.servers[i].Stop(); firstErr == nil && err != nil {
			firstErr = err
		}
	}
	for _, f := range ra.files {
		if err := f.Close(); firstErr == nil && err != nil {
			firstErr = err
		}
	}
	return firstErr
}

func splitNonRaidDevices(devices []db.NonRaidDeviceRow) (data, parity []db.NonRaidDeviceRow) {
	for _, dev := range devices {
		switch dev.Role {
		case RoleData:
			data = append(data, dev)
		case RoleParity:
			parity = append(parity, dev)
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i].Slot < data[j].Slot })
	sort.Slice(parity, func(i, j int) bool { return parity[i].Slot < parity[j].Slot })
	return data, parity
}

func runRuntimeCommand(name string, args ...string) error {
	out, err := runtimeCommand(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

func ensureRuntimeModule(name string) error {
	if kernelModuleLoaded(name) {
		return nil
	}
	return runRuntimeCommand("modprobe", name)
}

func kernelModuleLoaded(name string) bool {
	raw, err := os.ReadFile("/proc/modules")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func blockDeviceHasFilesystem(path string) bool {
	out, err := runtimeCommand("blkid", "-o", "value", "-s", "TYPE", path).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func makeFilesystem(fs, path string) error {
	switch fs {
	case "", DefaultFilesystem:
		return runRuntimeCommand("mkfs.xfs", "-f", path)
	default:
		return fmt.Errorf("unsupported nonRaid filesystem %q", fs)
	}
}

func mountDevice(device, mountPath string) error {
	if mountPath == "" {
		return fmt.Errorf("mount path required for %s", device)
	}
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mountPath, err)
	}
	if isMountpoint(mountPath) {
		return nil
	}
	return runRuntimeCommand("mount", "-o", "noatime", device, mountPath)
}

func mountSmoothfs(row *db.NonRaidArrayRow) error {
	if err := os.MkdirAll(row.MountPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", row.MountPath, err)
	}
	if isMountpoint(row.MountPath) {
		return nil
	}
	dataRows, _ := splitNonRaidDevices(row.Devices)
	tiers := make([]string, 0, len(dataRows))
	for _, dev := range dataRows {
		tiers = append(tiers, dev.MountPath)
	}
	opts := fmt.Sprintf("pool=%s,uuid=%s,tiers=%s,create_policy=high-water",
		row.Name, row.UUID, strings.Join(tiers, ":"))
	return runRuntimeCommand("mount", "-t", "smoothfs", "-o", opts, "none", row.MountPath)
}

func isMountpoint(path string) bool {
	return runtimeCommand("findmnt", "-M", path).Run() == nil
}

func unmount(path string) error {
	if err := runRuntimeCommand("umount", path); err == nil {
		return nil
	}
	return runRuntimeCommand("umount", "-l", path)
}

func findFreeNBD() (string, error) {
	for i := 0; i < 256; i++ {
		name := "nbd" + strconv.Itoa(i)
		pidPath := filepath.Join("/sys/block", name, "pid")
		raw, err := os.ReadFile(pidPath)
		if err == nil {
			pid := strings.TrimSpace(string(raw))
			if pid == "" || pid == "0" {
				return filepath.Join("/dev", name), nil
			}
			continue
		}
		sizePath := filepath.Join("/sys/block", name, "size")
		raw, err = os.ReadFile(sizePath)
		if err == nil && strings.TrimSpace(string(raw)) == "0" {
			return filepath.Join("/dev", name), nil
		}
	}
	return "", fmt.Errorf("no free /dev/nbd device found")
}

func disconnectNBD(path string) error {
	if err := DisconnectNBD(path); err != nil {
		log.Printf("nonraid: disconnect %s: %v", path, err)
		return err
	}
	return nil
}
