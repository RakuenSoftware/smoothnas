package network

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGenerateLinkFileMatchesPermanentMAC(t *testing.T) {
	got := GenerateLinkFile("net0", "c8:ff:bf:0e:37:d8")
	if !strings.Contains(got, "[Match]\nPermanentMACAddress=c8:ff:bf:0e:37:d8\n") {
		t.Fatalf("link file must match the permanent MAC:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "MACAddress=") {
			t.Fatalf("link file must not match the current MAC, which a bond can share:\n%s", got)
		}
	}
	if !strings.Contains(got, "[Link]\nName=net0\n") {
		t.Fatalf("link file must set the name:\n%s", got)
	}
}

// The installer's files, as on the Aoostar WTR Max that lost its network: the
// bond took net0's MAC and udev renamed bond0 to net0.
func TestMigrateLinkFilesToPermanentMAC(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	write("00-net0.link", "[Match]\nMACAddress=c8:ff:bf:0e:37:d8\n\n[Link]\nName=net0\n")
	write("00-net1.link", "# keep me\n[Match]\n  MACAddress = c8:ff:bf:0e:37:d9\n\n[Link]\nName=net1\n")
	already := "[Match]\nPermanentMACAddress=c8:ff:bf:0e:37:da\n\n[Link]\nName=net2\n"
	write("00-net2.link", already)
	custom := "[Match]\nMACAddress=c8:ff:bf:0e:37:db\nDriver=igc\n\n[Link]\nName=net3\n"
	write("50-custom.link", custom)
	bondNetdev := "[NetDev]\nName=bond0\nKind=bond\nMACAddress=c8:ff:bf:0e:37:d8\n"
	write("05-bond0.netdev", bondNetdev)

	migrated, err := MigrateLinkFilesToPermanentMAC(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"00-net0.link", "00-net1.link"}; !reflect.DeepEqual(migrated, want) {
		t.Fatalf("migrated = %v, want %v", migrated, want)
	}
	if got, want := read("00-net0.link"), "[Match]\nPermanentMACAddress=c8:ff:bf:0e:37:d8\n\n[Link]\nName=net0\n"; got != want {
		t.Fatalf("00-net0.link =\n%s\nwant\n%s", got, want)
	}
	if got, want := read("00-net1.link"), "# keep me\n[Match]\nPermanentMACAddress=c8:ff:bf:0e:37:d9\n\n[Link]\nName=net1\n"; got != want {
		t.Fatalf("00-net1.link =\n%s\nwant\n%s", got, want)
	}
	if got := read("00-net2.link"); got != already {
		t.Fatalf("already-migrated file changed:\n%s", got)
	}
	if got := read("50-custom.link"); got != custom {
		t.Fatalf("file with another selector changed:\n%s", got)
	}
	if got := read("05-bond0.netdev"); got != bondNetdev {
		t.Fatalf("netdev changed:\n%s", got)
	}

	again, err := MigrateLinkFilesToPermanentMAC(dir)
	if err != nil || len(again) != 0 {
		t.Fatalf("second migration = %v, %v; want no changes", again, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".*tmp*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestMigrateLinkFilesMissingDir(t *testing.T) {
	migrated, err := MigrateLinkFilesToPermanentMAC(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(migrated) != 0 {
		t.Fatalf("missing dir = %v, %v; want nothing", migrated, err)
	}
}

// linkMatchCurrentMACs returns the MACAddress= values in a .link file's
// [Match] section: the selectors udev compares against a device's current
// address, which a bond shares with its member.
func linkMatchCurrentMACs(content string) []string {
	var macs []string
	section := ""
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && section == "[Match]" && strings.TrimSpace(key) == "MACAddress" {
			macs = append(macs, strings.Fields(strings.ToLower(value))...)
		}
	}
	return macs
}

func netdevMAC(content string) string {
	for _, raw := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(raw), "=")
		if ok && key == "MACAddress" {
			return strings.ToLower(strings.TrimSpace(value))
		}
	}
	return ""
}

// The invariant behind the WTR Max outage: once tierd has reconciled the
// network directory, no NIC naming .link file may match the MAC that tierd
// pins on a bond, or udev renames the bond to that NIC's name at boot.
func TestBondMACNeverMatchesNICLinkFileAfterReconcile(t *testing.T) {
	store := newStubConfigStore()
	netDir := t.TempDir()
	sysRoot := makeFakeSysClassNet(t, map[string]string{
		"net0": "physical-eth",
		"net1": "physical-eth",
		"net2": "physical-eth",
		"net3": "physical-eth",
	})
	macs := map[string]string{
		"net0": "c8:ff:bf:0e:37:d8",
		"net1": "c8:ff:bf:0e:37:d9",
		"net2": "c8:ff:bf:0e:37:da",
		"net3": "c8:ff:bf:0e:37:db",
	}
	for name, mac := range macs {
		writeFakeFile(t, filepath.Join(sysRoot, name, "address"), mac+"\n")
		// What the installer wrote before it matched the permanent MAC.
		writeFakeFile(t, filepath.Join(netDir, "00-"+name+".link"),
			"[Match]\nMACAddress="+mac+"\n\n[Link]\nName="+name+"\n")
	}

	if err := ApplyDefaultBondPolicy(store, netDir, sysRoot); err != nil {
		t.Fatalf("apply default bond policy: %v", err)
	}
	// A bond the operator created in the UI, written by the API path.
	writeFakeFile(t, filepath.Join(netDir, "05-bond0.netdev"), GenerateBondNetdev(BondConfig{
		Name: "bond0", Mode: "active-backup", Members: []string{"net0", "net1"},
		MACAddress: MemberMAC(sysRoot, []string{"net0", "net1"}),
	}))

	if _, err := MigrateLinkFilesToPermanentMAC(netDir); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	entries, err := os.ReadDir(netDir)
	if err != nil {
		t.Fatal(err)
	}
	bondMACs := map[string]string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".netdev") {
			data, _ := os.ReadFile(filepath.Join(netDir, e.Name()))
			if mac := netdevMAC(string(data)); mac != "" {
				bondMACs[mac] = e.Name()
			}
		}
	}
	if len(bondMACs) == 0 {
		t.Fatal("expected the bond netdevs to pin a member MAC; the test would prove nothing")
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".link") {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(netDir, e.Name()))
		for _, mac := range linkMatchCurrentMACs(string(data)) {
			if netdev, ok := bondMACs[mac]; ok {
				t.Errorf("%s matches current MAC %s, which %s pins on a bond: udev would rename the bond", e.Name(), mac, netdev)
			}
		}
		if !strings.Contains(string(data), "PermanentMACAddress=") {
			t.Errorf("%s does not pin the permanent MAC:\n%s", e.Name(), data)
		}
	}
}

// GenerateLinkFile output must satisfy the same invariant for any MAC.
func TestGeneratedLinkFileNeverMatchesCurrentMAC(t *testing.T) {
	for _, mac := range []string{"c8:ff:bf:0e:37:d8", "bc:24:11:a1:b9:de", "00:15:5d:01:02:03"} {
		if got := linkMatchCurrentMACs(GenerateLinkFile("net0", mac)); len(got) != 0 {
			t.Fatalf("GenerateLinkFile(%s) matches current MAC %v", mac, got)
		}
	}
}
