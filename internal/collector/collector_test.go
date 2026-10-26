package collector

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/classify"
	"github.com/mohammadshamma/monitor-internet/internal/config"
	"github.com/mohammadshamma/monitor-internet/internal/discover"
	"github.com/mohammadshamma/monitor-internet/internal/report"
	"github.com/mohammadshamma/monitor-internet/internal/store"
)

// These are end-to-end tests through the real code path — real ICMP sockets,
// real SQLite, real classification and reporting. Unreachable tiers are
// simulated with TEST-NET-1 addresses (RFC 5737), which are guaranteed never to
// route, so no real network is disrupted.

const (
	unreachableA = "192.0.2.1"
	unreachableB = "192.0.2.2"
	unreachableC = "192.0.2.3"
	unreachableD = "192.0.2.4"
	reachable    = "1.1.1.1"
)

func newTestCollector(t *testing.T, targets []config.Target) (*Collector, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.Count = 1
	cfg.Timeout = time.Second

	c, err := New(cfg, st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.UseTargets(targets); err != nil {
		t.Fatalf("UseTargets: %v", err)
	}
	return c, st
}

func lastVerdict(t *testing.T, st *store.Store) string {
	t.Helper()
	var v string
	if err := st.DB().QueryRow(`SELECT verdict FROM cycles ORDER BY ts DESC LIMIT 1`).Scan(&v); err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	return v
}

// The single most important end-to-end guarantee: when the local router is
// unreachable, the tool must say so and must NOT record provider downtime.
func TestDeadGatewayIsNeverBlamedOnTheProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("live ICMP")
	}
	c, st := newTestCollector(t, []config.Target{
		{Name: "gateway", Addr: unreachableA, Tier: config.TierGateway},
		{Name: "isp_edge", Addr: reachable, Tier: config.TierISPEdge},
		{Name: "cloudflare", Addr: reachable, Tier: config.TierInternet},
	})

	ctx := context.Background()
	for i := 0; i < 4; i++ {
		c.Cycle(ctx)
	}

	if got := lastVerdict(t, st); got != string(classify.LANFault) {
		t.Fatalf("verdict = %s, want %s", got, classify.LANFault)
	}

	until := time.Now().Add(time.Minute)
	s, err := report.BuildSummary(st, until.Add(-time.Hour), until, config.Default().Interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if s.ISPDowntimeSeconds != 0 {
		t.Errorf("ISP downtime = %ds, want 0: a dead router is not the provider's fault",
			s.ISPDowntimeSeconds)
	}
	if s.ISPAvailabilityPct != 100 {
		t.Errorf("ISP availability = %.3f%%, want 100%%", s.ISPAvailabilityPct)
	}
	if s.LANDowntimeSeconds == 0 {
		t.Error("LAN downtime = 0; the local fault should still have been recorded")
	}
}

// The converse: router reachable, everything past it dead, is the provider's.
func TestDeadPathBeyondRouterBlamesTheProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("live ICMP")
	}
	c, st := newTestCollector(t, []config.Target{
		{Name: "gateway", Addr: reachable, Tier: config.TierGateway},
		{Name: "isp_edge", Addr: unreachableA, Tier: config.TierISPEdge},
		{Name: "cloudflare", Addr: unreachableB, Tier: config.TierInternet},
		{Name: "google", Addr: unreachableC, Tier: config.TierInternet},
		{Name: "quad9", Addr: unreachableD, Tier: config.TierInternet},
	})

	ctx := context.Background()
	for i := 0; i < 4; i++ {
		c.Cycle(ctx)
	}

	if got := lastVerdict(t, st); got != string(classify.ISPFault) {
		t.Fatalf("verdict = %s, want %s", got, classify.ISPFault)
	}

	until := time.Now().Add(time.Minute)
	s, err := report.BuildSummary(st, until.Add(-time.Hour), until, config.Default().Interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if s.ISPDowntimeSeconds == 0 {
		t.Error("ISP downtime = 0; a dead path beyond a live router is the provider's fault")
	}

	outages, err := report.Outages(st, until.Add(-time.Hour), until)
	if err != nil {
		t.Fatalf("Outages: %v", err)
	}
	if len(outages) != 1 {
		t.Fatalf("got %d outages, want 1 (debounce should have opened exactly one)", len(outages))
	}
	if !outages[0].BlamesISP {
		t.Errorf("outage class %q does not blame the provider", outages[0].Class)
	}
}

// A silent ISP edge hop while the internet works is cosmetic, not an outage —
// this is the false-positive that would otherwise fabricate a case.
func TestSilentISPHopIsNotAnOutage(t *testing.T) {
	if testing.Short() {
		t.Skip("live ICMP")
	}
	c, st := newTestCollector(t, []config.Target{
		{Name: "gateway", Addr: reachable, Tier: config.TierGateway},
		{Name: "isp_edge", Addr: unreachableA, Tier: config.TierISPEdge},
		{Name: "cloudflare", Addr: reachable, Tier: config.TierInternet},
	})

	ctx := context.Background()
	for i := 0; i < 4; i++ {
		c.Cycle(ctx)
	}

	if got := lastVerdict(t, st); got != string(classify.ICMPDeprio) {
		t.Fatalf("verdict = %s, want %s", got, classify.ICMPDeprio)
	}

	until := time.Now().Add(time.Minute)
	s, err := report.BuildSummary(st, until.Add(-time.Hour), until, config.Default().Interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if s.ISPDowntimeSeconds != 0 || s.OutageCount != 0 {
		t.Errorf("a quiet edge hop produced %ds downtime and %d outages; both must be 0",
			s.ISPDowntimeSeconds, s.OutageCount)
	}
}

// A single bad cycle must not create an outage record.
func TestSingleBadCycleCreatesNoOutageEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("live ICMP")
	}
	c, st := newTestCollector(t, []config.Target{
		{Name: "gateway", Addr: reachable, Tier: config.TierGateway},
		{Name: "cloudflare", Addr: unreachableA, Tier: config.TierInternet},
	})

	ctx := context.Background()
	c.Cycle(ctx) // one bad cycle only

	until := time.Now().Add(time.Minute)
	outages, err := report.Outages(st, until.Add(-time.Hour), until)
	if err != nil {
		t.Fatalf("Outages: %v", err)
	}
	if len(outages) != 0 {
		t.Errorf("got %d outages from a single bad cycle; debounce failed", len(outages))
	}
	s, err := report.BuildSummary(st, until.Add(-time.Hour), until, config.Default().Interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if s.ISPDowntimeSeconds != 0 {
		t.Errorf("ISP downtime = %ds from a single debounced cycle, want 0", s.ISPDowntimeSeconds)
	}
}

func TestPinnedGatewayDoesNotTriggerFalseLinkChangeRediscovery(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Gateway = "192.168.1.1" // outer router pinned in double-NAT
	cfg.ISPEdge = "203.0.113.1"

	c, err := New(cfg, st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	defer c.Close()

	var discoverCalls int
	c.defaultRoute = func(context.Context) (discover.Route, error) {
		return discover.Route{Gateway: "192.168.2.1", Interface: "en0"}, nil
	}
	c.lookupSSID = func(context.Context, string) string { return "HomeWiFi" }
	c.discover = func(ctx context.Context) error {
		discoverCalls++
		return c.Discover(ctx)
	}

	ctx := context.Background()
	if err := c.Discover(ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if c.gateway != "192.168.1.1" {
		t.Fatalf("c.gateway = %q, want 192.168.1.1", c.gateway)
	}

	// Subsequent refreshLink ticks see the same OS default route (192.168.2.1)
	// and must not mistake the pinned gateway override for a link change.
	c.refreshLink(ctx)
	c.refreshLink(ctx)
	if discoverCalls != 0 {
		t.Errorf("refreshLink triggered %d rediscoveries when gateway was pinned, want 0", discoverCalls)
	}
}

func TestDiscoverClearsStaleISPEdgeWhenFirstISPHopFails(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	c, err := New(cfg, st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	defer c.Close()

	c.defaultRoute = func(context.Context) (discover.Route, error) {
		return discover.Route{Gateway: "192.168.1.1", Interface: "en0"}, nil
	}
	c.lookupSSID = func(context.Context, string) string { return "HomeWiFi" }
	c.ispEdge = "203.0.113.9"
	c.firstISPHop = func(context.Context, string, string) ([]string, error) {
		return nil, errors.New("no public hop found")
	}

	if err := c.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if c.ispEdge != "" {
		t.Errorf("c.ispEdge = %q, want empty after FirstISPHop failure", c.ispEdge)
	}
	if got := st.GetMeta("isp_edge"); got != "" {
		t.Errorf("meta isp_edge = %q, want empty", got)
	}
}

func TestPickResponsiveHopNeverSelectsTraceTargetOrAnchor(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Timeout = 100 * time.Millisecond
	c, err := New(cfg, st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	defer c.Close()

	// If an intermediate ISP hop drops ICMP and 1.1.1.1 (TraceTarget/anchor) is
	// in candidates, pickResponsiveHop must not collapse tier separation by
	// picking 1.1.1.1 as isp_edge.
	got := c.pickResponsiveHop(context.Background(), []string{unreachableA, "1.1.1.1"})
	if got != unreachableA {
		t.Errorf("pickResponsiveHop = %q, want %q (must exclude TraceTarget 1.1.1.1)", got, unreachableA)
	}
	if onlyTarget := c.pickResponsiveHop(context.Background(), []string{"1.1.1.1"}); onlyTarget != "" {
		t.Errorf("pickResponsiveHop([1.1.1.1]) = %q, want empty", onlyTarget)
	}
}
