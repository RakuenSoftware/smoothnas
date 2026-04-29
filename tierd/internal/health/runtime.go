package health

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/nfs"
	"github.com/JBailes/SmoothNAS/tierd/internal/smb"
)

const sambaVFSGuardPath = "/etc/apt/preferences.d/smoothnas-samba-vfs"

func RuntimeChecks(store *db.Store) CheckProvider {
	return func(ctx context.Context) []Check {
		_ = ctx
		mounts := smoothfsMounts()
		checks := []Check{
			sambaVFSCheck(store, mounts),
			sambaUpgradeGuardCheck(),
		}
		checks = append(checks, nfsTuningChecks(store)...)
		return filterEmptyChecks(checks)
	}
}

func sambaVFSCheck(store *db.Store, mounts []string) Check {
	shares, _ := store.ListSmbShares()
	needsVFS := len(mounts) > 0
	for _, share := range shares {
		if pathUnderAnyMount(share.Path, mounts) {
			needsVFS = true
			break
		}
	}
	if !needsVFS {
		return Check{}
	}
	if smb.SmoothFSVFSInstalled() {
		return Check{Name: "smoothfs-samba-vfs", Status: "ok", Message: strings.Join(smb.SmoothFSVFSPaths(), ", ")}
	}
	if len(shares) > 0 {
		return Check{Name: "smoothfs-samba-vfs", Status: "critical", Message: "SMB has SmoothFS paths but /usr/lib/*/samba/vfs/smoothfs.so is missing"}
	}
	return Check{Name: "smoothfs-samba-vfs", Status: "warning", Message: "SmoothFS is mounted but the Samba VFS module is missing"}
}

func sambaUpgradeGuardCheck() Check {
	if !smb.SmoothFSVFSInstalled() {
		return Check{}
	}
	if _, err := os.Stat(sambaVFSGuardPath); err != nil {
		return Check{Name: "samba-vfs-abi-guard", Status: "warning", Message: "Samba VFS module is present but apt upgrade pin is missing"}
	}
	sambaVersion, ok := packageVersion("samba")
	if !ok {
		return Check{Name: "samba-vfs-abi-guard", Status: "warning", Message: "Samba VFS module is present but samba package version could not be read"}
	}
	raw, err := os.ReadFile(sambaVFSGuardPath)
	if err != nil {
		return Check{Name: "samba-vfs-abi-guard", Status: "warning", Message: "Samba VFS apt guard is unreadable"}
	}
	if !strings.Contains(string(raw), "Pin: version "+sambaVersion) {
		return Check{Name: "samba-vfs-abi-guard", Status: "warning", Message: "Samba VFS apt guard does not match installed samba version " + sambaVersion}
	}
	return Check{Name: "samba-vfs-abi-guard", Status: "ok", Message: "Samba packages pinned at " + sambaVersion}
}

func nfsTuningChecks(store *db.Store) []Check {
	exports, _ := store.ListNfsExports()
	if len(exports) == 0 && !nfs.IsEnabled() {
		return nil
	}
	var checks []Check
	if value, ok := nfs.NFSMaxBlockSize(); ok {
		checks = append(checks, minIntCheck("nfsd-max-block-size", value, fmt.Sprint(nfs.MaxBlockSize)))
	}
	if value, ok := nfs.SunRPCSlotTableEntries(); ok {
		checks = append(checks, minIntCheck("sunrpc-tcp-slot-table", value, fmt.Sprint(nfs.ClientSunRPCSlots)))
	}
	for _, path := range nfs.TuningFiles() {
		if _, err := os.Stat(path); err != nil {
			checks = append(checks, Check{Name: "nfs-tuning-file", Status: "warning", Message: path + " is missing"})
		}
	}
	return checks
}

func minIntCheck(name, got, want string) Check {
	gotN, gotErr := strconv.ParseInt(strings.TrimSpace(got), 10, 64)
	wantN, wantErr := strconv.ParseInt(strings.TrimSpace(want), 10, 64)
	if gotErr != nil || wantErr != nil {
		return Check{Name: name, Status: "warning", Message: fmt.Sprintf("cannot parse value %q; expected at least %s", got, want)}
	}
	if gotN < wantN {
		return Check{Name: name, Status: "warning", Message: fmt.Sprintf("%d is below SmoothNAS target %d", gotN, wantN)}
	}
	return Check{Name: name, Status: "ok", Message: fmt.Sprintf("%d", gotN)}
}

func smoothfsMounts() []string {
	raw, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	var mounts []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == "smoothfs" {
			mounts = append(mounts, fields[1])
		}
	}
	return mounts
}

func pathUnderAnyMount(path string, mounts []string) bool {
	path = filepath.Clean(path)
	for _, mount := range mounts {
		mount = filepath.Clean(mount)
		if path == mount || strings.HasPrefix(path, mount+"/") {
			return true
		}
	}
	return false
}

func filterEmptyChecks(checks []Check) []Check {
	out := checks[:0]
	for _, check := range checks {
		if check.Name != "" {
			out = append(out, check)
		}
	}
	return out
}

func packageVersion(name string) (string, bool) {
	out, err := exec.Command("dpkg-query", "-W", "-f=${Version}", name).CombinedOutput()
	if err != nil {
		return "", false
	}
	version := strings.TrimSpace(string(out))
	return version, version != ""
}
