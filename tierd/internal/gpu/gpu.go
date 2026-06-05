package gpu

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Device describes a GPU-ish accelerator device that can be offered in
// install-time plugin config. DevicePath is the path SmoothNAS should pass
// through for a selected GPU.
type Device struct {
	ID         string `json:"id"`
	Vendor     string `json:"vendor"`
	Name       string `json:"name"`
	DevicePath string `json:"devicePath"`
	RenderNode string `json:"renderNode,omitempty"`
	CardNode   string `json:"cardNode,omitempty"`
	PCIAddress string `json:"pciAddress,omitempty"`
}

var nvidiaNodeRe = regexp.MustCompile(`^/dev/nvidia([0-9]+)$`)

// List returns host GPU devices visible through stable /dev paths. It is
// intentionally best-effort: missing subsystems return an empty slice rather
// than blocking the plugin installer.
func List() ([]Device, error) {
	var devices []Device
	devices = append(devices, listNVIDIA()...)
	devices = append(devices, listDRM()...)
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Vendor != devices[j].Vendor {
			return devices[i].Vendor < devices[j].Vendor
		}
		return devices[i].DevicePath < devices[j].DevicePath
	})
	return devices, nil
}

func listNVIDIA() []Device {
	paths, _ := filepath.Glob("/dev/nvidia[0-9]*")
	out := make([]Device, 0, len(paths))
	for _, path := range paths {
		if !isCharDevice(path) {
			continue
		}
		match := nvidiaNodeRe.FindStringSubmatch(path)
		if len(match) != 2 {
			continue
		}
		id := "nvidia" + match[1]
		out = append(out, Device{
			ID:         id,
			Vendor:     "nvidia",
			Name:       "NVIDIA GPU " + match[1],
			DevicePath: path,
		})
	}
	return out
}

func listDRM() []Device {
	paths, _ := filepath.Glob("/dev/dri/renderD*")
	out := make([]Device, 0, len(paths))
	for _, render := range paths {
		if !isCharDevice(render) {
			continue
		}
		node := filepath.Base(render)
		sysDevice := filepath.Join("/sys/class/drm", node, "device")
		vendor := vendorFromHex(readTrimmed(filepath.Join(sysDevice, "vendor")))
		if vendor == "" {
			vendor = "unknown"
		}
		card := drmCardNode(sysDevice)
		pci := pciAddress(sysDevice)
		name := strings.ToUpper(vendor[:1]) + vendor[1:] + " GPU " + node
		out = append(out, Device{
			ID:         node,
			Vendor:     vendor,
			Name:       name,
			DevicePath: render,
			RenderNode: render,
			CardNode:   card,
			PCIAddress: pci,
		})
	}
	return out
}

func isCharDevice(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Mode&syscall.S_IFMT == syscall.S_IFCHR
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func vendorFromHex(hex string) string {
	switch strings.ToLower(hex) {
	case "0x10de":
		return "nvidia"
	case "0x1002", "0x1022":
		return "amd"
	case "0x8086":
		return "intel"
	default:
		return ""
	}
}

func drmCardNode(sysDevice string) string {
	matches, _ := filepath.Glob(filepath.Join(sysDevice, "drm", "card[0-9]*"))
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return filepath.Join("/dev/dri", filepath.Base(matches[0]))
}

// PrimaryCardNode returns the primary DRM node (/dev/dri/cardN) that shares a
// physical GPU with the given render node (/dev/dri/renderDM), or "" if it
// can't be resolved. Compositors such as games-on-whales Wolf enumerate a GPU
// from its primary node; passing only the render node leaves them unable to
// drive the device — Wolf logs "doesn't have a primary node" and exposes no
// GPU to the app containers it launches.
func PrimaryCardNode(renderNode string) string {
	node := filepath.Base(renderNode)
	if !strings.HasPrefix(node, "renderD") {
		return ""
	}
	return drmCardNode(filepath.Join("/sys/class/drm", node, "device"))
}

func pciAddress(sysDevice string) string {
	resolved, err := filepath.EvalSymlinks(sysDevice)
	if err != nil {
		return ""
	}
	base := filepath.Base(resolved)
	if strings.Count(base, ":") >= 2 {
		return base
	}
	return ""
}
