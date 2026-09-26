package network

import (
	"net"
	"testing"
)

func TestNextFree(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.88.0.0/29") // .0 network, .7 broadcast
	gw := net.ParseIP("10.88.0.1")
	taken := map[string]string{}
	var got []string
	for {
		ip, err := nextFree(subnet, gw, taken)
		if err != nil {
			break
		}
		taken[ip.String()] = "c"
		got = append(got, ip.String())
	}
	want := []string{"10.88.0.2", "10.88.0.3", "10.88.0.4", "10.88.0.5", "10.88.0.6"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// A released address is reused first.
	delete(taken, "10.88.0.3")
	if ip, _ := nextFree(subnet, gw, taken); ip.String() != "10.88.0.3" {
		t.Errorf("got %v, want 10.88.0.3", ip)
	}
}
