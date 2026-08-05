// Package discover works out the local network topology: the default gateway,
// the first hop inside the ISP's network, and which interface is carrying
// traffic.
//
// These shell out to macOS system binaries by absolute path. That is
// deliberate on two counts: launchd hands agents a minimal PATH, and these
// binaries ship with the OS, so no package manager can remove them. They also
// run rarely — at startup and on periodic re-verification — never in the
// timing-critical probe path.
package discover

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

const (
	routeBin        = "/sbin/route"
	tracerouteBin   = "/usr/sbin/traceroute"
	networksetupBin = "/usr/sbin/networksetup"
)

// Route describes the current default route.
type Route struct {
	Gateway   string
	Interface string
}

// DefaultRoute returns the current default gateway and its interface.
func DefaultRoute(ctx context.Context) (Route, error) {
	out, err := exec.CommandContext(ctx, routeBin, "-n", "get", "default").Output()
	if err != nil {
		return Route{}, fmt.Errorf("route -n get default: %w", err)
	}

	var r Route
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		field, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch field {
		case "gateway":
			r.Gateway = value
		case "interface":
			r.Interface = value
		}
	}
	if r.Gateway == "" {
		return Route{}, fmt.Errorf("no default gateway in route output")
	}
	return r, nil
}

// Hop is one entry from a traceroute.
type Hop struct {
	Number int
	Addr   string
}

// Trace runs a short traceroute towards target and returns the hops that
// answered.
func Trace(ctx context.Context, target string, maxHops int) ([]Hop, error) {
	cmd := exec.CommandContext(ctx, tracerouteBin,
		"-n", "-q", "1", "-m", fmt.Sprint(maxHops), "-w", "2", target)
	out, err := cmd.Output()
	if err != nil {
		// traceroute exits non-zero in some partial-path cases but still prints
		// usable hops, so parse whatever we got before giving up.
		if len(out) == 0 {
			return nil, fmt.Errorf("traceroute %s: %w", target, err)
		}
	}

	var hops []Hop
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		var num int
		if _, err := fmt.Sscanf(fields[0], "%d", &num); err != nil {
			continue // header line
		}
		if ip := net.ParseIP(fields[1]); ip != nil {
			hops = append(hops, Hop{Number: num, Addr: fields[1]})
		}
	}
	return hops, nil
}

// FirstISPHop returns the first hop past the gateway that is not a private
// address — the edge of the provider's network, and the point that separates
// "my house" from "their line".
//
// It returns candidates in path order so a caller can fall back to a later hop
// when the first one refuses to answer ICMP.
func FirstISPHop(ctx context.Context, gateway, target string) ([]string, error) {
	hops, err := Trace(ctx, target, 6)
	if err != nil {
		return nil, err
	}

	var candidates []string
	for _, h := range hops {
		if h.Addr == gateway {
			continue
		}
		ip := net.ParseIP(h.Addr)
		if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		candidates = append(candidates, h.Addr)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no public hop found beyond gateway %s", gateway)
	}
	return candidates, nil
}

// SSID returns the Wi-Fi network name for an interface, or "" when the
// interface is not wireless or not associated.
//
// This is recorded per cycle because a Wi-Fi change invalidates comparison
// across time — and because Wi-Fi being in the path at all is a variable the
// gateway tier exists to isolate.
func SSID(ctx context.Context, iface string) string {
	if iface == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, networksetupBin, "-getairportnetwork", iface).Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	_, name, ok := strings.Cut(line, ": ")
	if !ok || strings.Contains(line, "not associated") {
		return ""
	}
	return strings.TrimSpace(name)
}
