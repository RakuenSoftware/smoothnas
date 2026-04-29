package nfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyServerTuningWritesPersistentFiles(t *testing.T) {
	dir := t.TempDir()
	old := []string{
		serverTuningPath,
		lockdSysctlPath,
		lockdModprobePath,
		nfsdDropInPath,
		nfsKernelDropInPath,
		procNFSMaxBlockPath,
		procSunRPCSlotTable,
		procLockdTCPPortPath,
		procLockdUDPPortPath,
	}
	t.Cleanup(func() {
		serverTuningPath = old[0]
		lockdSysctlPath = old[1]
		lockdModprobePath = old[2]
		nfsdDropInPath = old[3]
		nfsKernelDropInPath = old[4]
		procNFSMaxBlockPath = old[5]
		procSunRPCSlotTable = old[6]
		procLockdTCPPortPath = old[7]
		procLockdUDPPortPath = old[8]
	})

	serverTuningPath = filepath.Join(dir, "nfs.conf.d", "smoothnas.conf")
	lockdSysctlPath = filepath.Join(dir, "sysctl.d", "99-smoothnas-nfs.conf")
	lockdModprobePath = filepath.Join(dir, "modprobe.d", "smoothnas-lockd.conf")
	nfsdDropInPath = filepath.Join(dir, "systemd", "nfs-server.service.d", "10-smoothnas-tuning.conf")
	nfsKernelDropInPath = filepath.Join(dir, "systemd", "nfs-kernel-server.service.d", "10-smoothnas-tuning.conf")
	procNFSMaxBlockPath = filepath.Join(dir, "proc", "max_block_size")
	procSunRPCSlotTable = filepath.Join(dir, "sys", "tcp_slot_table_entries")
	procLockdTCPPortPath = filepath.Join(dir, "missing", "nlm_tcpport")
	procLockdUDPPortPath = filepath.Join(dir, "missing", "nlm_udpport")

	if err := os.MkdirAll(filepath.Dir(procNFSMaxBlockPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(procSunRPCSlotTable), 0755); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{
		procNFSMaxBlockPath: "1048576\n",
		procSunRPCSlotTable: "2\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := ApplyServerTuning(); err != nil {
		t.Fatalf("ApplyServerTuning: %v", err)
	}

	assertFileContains(t, serverTuningPath, "threads = 32")
	assertFileContains(t, lockdSysctlPath, "fs.nfs.nlm_tcpport = 32767")
	assertFileContains(t, nfsdDropInPath, "2097152")
	assertFileContains(t, nfsKernelDropInPath, "2097152")
	assertFileContains(t, procNFSMaxBlockPath, fmt.Sprint(MaxBlockSize))
	assertFileContains(t, procSunRPCSlotTable, fmt.Sprint(ClientSunRPCSlots))
}

func assertFileContains(t *testing.T, path, needle string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), needle) {
		t.Fatalf("%s does not contain %q:\n%s", path, needle, raw)
	}
}
