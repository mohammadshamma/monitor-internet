package discover

import (
	"context"
	"net"
	"testing"
)

func TestDefaultRouteLive(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the live system routing table")
	}
	r, err := DefaultRoute(context.Background())
	if err != nil {
		t.Fatalf("DefaultRoute: %v", err)
	}
	if net.ParseIP(r.Gateway) == nil {
		t.Errorf("gateway %q is not an IP", r.Gateway)
	}
	if r.Interface == "" {
		t.Error("no interface reported")
	}
	t.Logf("gateway=%s iface=%s ssid=%q", r.Gateway, r.Interface, SSID(context.Background(), r.Interface))
}

func TestFirstISPHopLive(t *testing.T) {
	if testing.Short() {
		t.Skip("needs live traceroute")
	}
	r, err := DefaultRoute(context.Background())
	if err != nil {
		t.Fatalf("DefaultRoute: %v", err)
	}
	hops, err := FirstISPHop(context.Background(), r.Gateway, "1.1.1.1")
	if err != nil {
		t.Fatalf("FirstISPHop: %v", err)
	}
	for _, h := range hops {
		ip := net.ParseIP(h)
		if ip == nil {
			t.Errorf("hop %q is not an IP", h)
			continue
		}
		if ip.IsPrivate() {
			t.Errorf("hop %s is a private address; it is not past the ISP edge", h)
		}
		if h == r.Gateway {
			t.Errorf("gateway %s was returned as an ISP hop", h)
		}
	}
	t.Logf("ISP hop candidates: %v", hops)
}
