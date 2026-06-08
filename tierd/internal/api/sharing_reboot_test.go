package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/smb"
)

// stubProtocolServices replaces the protocol enable/disable service calls with
// no-op recorders so toggle/reapply can be exercised without a live systemd.
// It returns pointers to per-protocol enable counters.
func stubProtocolServices(t *testing.T) (smbEnables, nfsEnables, iscsiEnables *int) {
	t.Helper()
	smbEnables, nfsEnables, iscsiEnables = new(int), new(int), new(int)

	origSMBEnable, origSMBDisable := enableSMBService, disableSMBService
	origNFSEnable, origNFSDisable := enableNFSServiceForExports, disableNFSService
	origISCSIEnable, origISCSIDisable := enableISCSIService, disableISCSIService
	origWriteSMB := writeSMBConfig
	origApplyFirewall, origEnabledProtocols := applyFirewallForExports, enabledProtocolsForExports
	t.Cleanup(func() {
		enableSMBService, disableSMBService = origSMBEnable, origSMBDisable
		enableNFSServiceForExports, disableNFSService = origNFSEnable, origNFSDisable
		enableISCSIService, disableISCSIService = origISCSIEnable, origISCSIDisable
		writeSMBConfig = origWriteSMB
		applyFirewallForExports, enabledProtocolsForExports = origApplyFirewall, origEnabledProtocols
	})

	enableSMBService = func() error { *smbEnables++; return nil }
	disableSMBService = func() error { return nil }
	enableNFSServiceForExports = func(bool) error { *nfsEnables++; return nil }
	disableNFSService = func() error { return nil }
	enableISCSIService = func() error { *iscsiEnables++; return nil }
	disableISCSIService = func() error { return nil }
	writeSMBConfig = func([]smb.Share, string, smb.Options) error { return nil }
	applyFirewallForExports = func(map[string]bool) error { return nil }
	enabledProtocolsForExports = func() map[string]bool { return map[string]bool{} }
	return
}

func TestToggleProtocolPersistsEnabledState(t *testing.T) {
	h := newTestSharingHandler(t)
	stubProtocolServices(t)

	toggle := func(proto, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/protocols/"+proto, strings.NewReader(body))
		w := httptest.NewRecorder()
		h.Route(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("toggle %s: expected 200, got %d: %s", proto, w.Code, w.Body.String())
		}
	}

	toggle("smb", `{"enabled":true}`)
	toggle("nfs", `{"enabled":true}`)
	toggle("iscsi", `{"enabled":false}`)

	for _, tc := range []struct {
		key  string
		want bool
	}{
		{smbEnabledConfigKey, true},
		{nfsEnabledConfigKey, true},
		{iscsiEnabledConfigKey, false},
	} {
		got, err := h.store.GetBoolConfig(tc.key, false)
		if err != nil {
			t.Fatalf("read %s: %v", tc.key, err)
		}
		if got != tc.want {
			t.Fatalf("%s = %t, want %t", tc.key, got, tc.want)
		}
	}
}

func TestReapplySharingServicesReenablesPersistedProtocols(t *testing.T) {
	h := newTestSharingHandler(t)
	smbEnables, nfsEnables, iscsiEnables := stubProtocolServices(t)

	// Operator had SMB and NFS on, iSCSI never enabled.
	if err := h.store.SetBoolConfig(smbEnabledConfigKey, true); err != nil {
		t.Fatalf("persist smb: %v", err)
	}
	if err := h.store.SetBoolConfig(nfsEnabledConfigKey, true); err != nil {
		t.Fatalf("persist nfs: %v", err)
	}

	ReapplySharingServices(h.store)

	if *smbEnables != 1 {
		t.Errorf("smb enables = %d, want 1", *smbEnables)
	}
	if *nfsEnables != 1 {
		t.Errorf("nfs enables = %d, want 1", *nfsEnables)
	}
	if *iscsiEnables != 0 {
		t.Errorf("iscsi enables = %d, want 0 (never enabled, keep installer baseline)", *iscsiEnables)
	}
}

func TestReapplySharingServicesNoStateIsNoop(t *testing.T) {
	h := newTestSharingHandler(t)
	smbEnables, nfsEnables, iscsiEnables := stubProtocolServices(t)

	ReapplySharingServices(h.store)

	if *smbEnables != 0 || *nfsEnables != 0 || *iscsiEnables != 0 {
		t.Fatalf("expected no service calls on a fresh appliance, got smb=%d nfs=%d iscsi=%d",
			*smbEnables, *nfsEnables, *iscsiEnables)
	}
}
