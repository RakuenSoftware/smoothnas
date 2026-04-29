package network

import "testing"

func TestReadProcNetDevParsesEthernetRows(t *testing.T) {
	const sample = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 3580228597 11897958    0    0    0     0          0         0 3580228597 11897958    0    0    0     0       0          0
  eth0: 2152242114 8683031    0   33    0     0          0         0 17939428175 7058565    0    0    0     0       0          0
  eth1:        0       0    0    0    0     0          0         0           0       0    0    0    0     0       0          0
`
	got := readProcNetDev(sample)
	if len(got) != 3 {
		t.Fatalf("want 3 rows (lo + eth0 + eth1), got %d: %v", len(got), got)
	}

	eth0 := got["eth0"]
	if eth0.RxBytes != 2152242114 {
		t.Fatalf("eth0 RxBytes = %d, want 2152242114", eth0.RxBytes)
	}
	if eth0.RxPackets != 8683031 {
		t.Fatalf("eth0 RxPackets = %d, want 8683031", eth0.RxPackets)
	}
	if eth0.RxDrop != 33 {
		t.Fatalf("eth0 RxDrop = %d, want 33", eth0.RxDrop)
	}
	if eth0.TxBytes != 17939428175 {
		t.Fatalf("eth0 TxBytes = %d, want 17939428175", eth0.TxBytes)
	}
	if eth0.TxPackets != 7058565 {
		t.Fatalf("eth0 TxPackets = %d, want 7058565", eth0.TxPackets)
	}

	lo := got["lo"]
	if lo.Name != "lo" {
		t.Fatalf("lo row missing name field")
	}

	eth1 := got["eth1"]
	if eth1.RxBytes != 0 || eth1.TxBytes != 0 {
		t.Fatalf("eth1 zero counters not preserved: %+v", eth1)
	}
}

func TestReadProcNetDevSkipsHeaderlessOrShortLines(t *testing.T) {
	got := readProcNetDev("Inter-| Receive | Transmit\n")
	if len(got) != 0 {
		t.Fatalf("header-only input should yield 0 rows, got %d", len(got))
	}
	got = readProcNetDev("not-a-row-without-colon\n")
	if len(got) != 0 {
		t.Fatalf("colonless line should be skipped, got %v", got)
	}
}

func TestParseUint64HandlesBadInput(t *testing.T) {
	if got := parseUint64(""); got != 0 {
		t.Fatalf("empty string -> %d, want 0", got)
	}
	if got := parseUint64("abc"); got != 0 {
		t.Fatalf("non-numeric -> %d, want 0", got)
	}
	if got := parseUint64("42"); got != 42 {
		t.Fatalf("42 -> %d, want 42", got)
	}
}

func TestReadEstablishedConnsForIPsHandlesEmpty(t *testing.T) {
	if got := readEstablishedConnsForIPs(nil); got != 0 {
		t.Fatalf("nil ips -> %d, want 0", got)
	}
	if got := readEstablishedConnsForIPs([]string{}); got != 0 {
		t.Fatalf("empty ips -> %d, want 0", got)
	}
}
