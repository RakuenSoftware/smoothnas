package compose

import (
	"sort"
	"testing"
)

func TestHostPorts(t *testing.T) {
	yaml := `
name: demo
services:
  web:
    image: nginx
    ports:
      - "8080:80"
      - "127.0.0.1:8443:443"
      - "53:53/udp"
      - "9000"          # container-only, ignored
      - target: 9090
        published: 19090
        protocol: tcp
      - "7000-7005:7000-7005"  # range, skipped
  side:
    image: busybox
    ports:
      - "6000:6000"
`
	got, err := HostPorts([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for _, h := range got {
		keys = append(keys, h.Key())
	}
	sort.Strings(keys)
	want := []string{"19090/tcp", "53/udp", "6000/tcp", "8080/tcp", "8443/tcp"}
	if len(keys) != len(want) {
		t.Fatalf("got %v want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got %v want %v", keys, want)
		}
	}
}

func TestHostPorts_EmptyAndNoPorts(t *testing.T) {
	if p, err := HostPorts([]byte("services:\n  x:\n    image: a\n")); err != nil || p != nil {
		t.Fatalf("no ports => nil, got %v err %v", p, err)
	}
}

func TestHostPorts_IPv6AndSCTP(t *testing.T) {
	y := "services:\n  s:\n    image: a\n    ports:\n" +
		"      - \"[::1]:8081:80\"\n" +
		"      - \"5000:5000/sctp\"\n" +
		"      - \"[2001:db8::1]:9443:443/udp\"\n"
	got, err := HostPorts([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, h := range got {
		keys[h.Key()] = true
	}
	for _, want := range []string{"8081/tcp", "5000/sctp", "9443/udp"} {
		if !keys[want] {
			t.Fatalf("missing %s in %v", want, keys)
		}
	}
}
