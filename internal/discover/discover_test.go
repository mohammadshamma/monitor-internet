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
		if h == "1.1.1.1" {
			t.Errorf("trace target 1.1.1.1 was returned as an ISP hop")
		}
	}
	t.Logf("ISP hop candidates: %v", hops)
}

func TestFirstISPHopExcludesTraceTarget(t *testing.T) {
	orig := traceHops
	t.Cleanup(func() { traceHops = orig })

	traceHops = func(_ context.Context, _ string, _ int) ([]Hop, error) {
		return []Hop{
			{Number: 1, Addr: "192.168.1.1"},
			{Number: 2, Addr: "10.0.0.1"},
			{Number: 3, Addr: "203.0.113.1"},
			{Number: 4, Addr: "1.1.1.1"},
		}, nil
	}

	got, err := FirstISPHop(context.Background(), "192.168.1.1", "1.1.1.1")
	if err != nil {
		t.Fatalf("FirstISPHop: %v", err)
	}
	if len(got) != 1 || got[0] != "203.0.113.1" {
		t.Fatalf("FirstISPHop = %v, want [203.0.113.1] (must exclude target 1.1.1.1)", got)
	}
}

func TestFirstISPHopErrorsWhenOnlyTargetIsPublic(t *testing.T) {
	orig := traceHops
	t.Cleanup(func() { traceHops = orig })

	traceHops = func(_ context.Context, _ string, _ int) ([]Hop, error) {
		return []Hop{
			{Number: 1, Addr: "192.168.1.1"},
			{Number: 2, Addr: "10.0.0.1"},
			{Number: 3, Addr: "1.1.1.1"},
		}, nil
	}

	if got, err := FirstISPHop(context.Background(), "192.168.1.1", "1.1.1.1"); err == nil {
		t.Fatalf("FirstISPHop = %v, want error when only public hop is target", got)
	}
}
