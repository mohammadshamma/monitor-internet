// Package collector runs the probe loop: discover topology, probe every tier
// on a fixed interval, classify, and persist.
package collector

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/classify"
	"github.com/mohammadshamma/monitor-internet/internal/config"
	"github.com/mohammadshamma/monitor-internet/internal/discover"
	"github.com/mohammadshamma/monitor-internet/internal/probe"
	"github.com/mohammadshamma/monitor-internet/internal/store"
)

// Collector owns the probe loop and its state.
type Collector struct {
	cfg    config.Config
	st     *store.Store
	prober *probe.Prober
	track  *classify.Tracker
	log    *log.Logger

	targets   []config.Target
	targetIDs map[string]int64

	gateway string
	ispEdge string
	iface   string
	ssid    string

	openOutageID int64
}

// New builds a Collector against an already-open store.
func New(cfg config.Config, st *store.Store, lg *log.Logger) (*Collector, error) {
	p, err := probe.New()
	if err != nil {
		return nil, err
	}
	return &Collector{
		cfg:       cfg,
		st:        st,
		prober:    p,
		track:     classify.NewTracker(cfg.FailThreshold, cfg.ClearThreshold),
		log:       lg,
		targetIDs: make(map[string]int64),
	}, nil
}

// Close releases the ICMP socket.
func (c *Collector) Close() error { return c.prober.Close() }

// Targets returns the registered probe targets, in tier order.
func (c *Collector) Targets() []config.Target { return c.targets }

// UseTargets registers an explicit target set instead of discovering one.
//
// This exists for two reasons: end-to-end tests need to point a tier at a known
// unreachable address to prove the attribution rules hold through the real code
// path, and an unusual network (double NAT, a router that never answers ICMP)
// may need the topology pinned by hand.
func (c *Collector) UseTargets(targets []config.Target) error {
	c.gateway, c.ispEdge = "", ""
	for _, t := range targets {
		switch t.Tier {
		case config.TierGateway:
			c.gateway = t.Addr
		case config.TierISPEdge:
			c.ispEdge = t.Addr
		}
	}
	c.targets = targets
	for _, t := range c.targets {
		id, err := c.st.UpsertTarget(t.Name, t.Addr, string(t.Tier))
		if err != nil {
			return fmt.Errorf("register target %s: %w", t.Name, err)
		}
		c.targetIDs[t.Name] = id
	}
	_ = c.st.SetMeta("gateway", c.gateway)
	_ = c.st.SetMeta("isp_edge", c.ispEdge)
	return nil
}

// Cycle runs a single probe round and persists it. Run calls this on a ticker;
// it is exported so tests can drive one cycle deterministically.
func (c *Collector) Cycle(ctx context.Context) { c.cycle(ctx) }

// Discover works out the topology and registers the resulting targets.
func (c *Collector) Discover(ctx context.Context) error {
	route, err := discover.DefaultRoute(ctx)
	if err != nil {
		return fmt.Errorf("discover default route: %w", err)
	}
	c.iface = route.Interface
	c.ssid = discover.SSID(ctx, route.Interface)

	// A configured gateway or ISP edge pins the topology by hand, for networks
	// where discovery cannot work (double NAT, a router that never answers).
	c.gateway = route.Gateway
	if c.cfg.Gateway != "" {
		c.gateway = c.cfg.Gateway
		c.log.Printf("gateway pinned by config: %s", c.gateway)
	}

	switch {
	case c.cfg.ISPEdge != "":
		c.ispEdge = c.cfg.ISPEdge
		c.log.Printf("ISP edge pinned by config: %s", c.ispEdge)
	default:
		candidates, err := discover.FirstISPHop(ctx, c.gateway, c.cfg.TraceTarget)
		if err != nil {
			// Losing the ISP edge target degrades attribution detail but must
			// not stop collection — gateway and internet tiers still separate
			// "my house" from "not my house".
			c.log.Printf("warn: could not find ISP edge hop: %v", err)
		} else {
			c.ispEdge = c.pickResponsiveHop(ctx, candidates)
		}
	}

	return c.registerTargets()
}

// pickResponsiveHop chooses the first candidate hop that actually answers ICMP.
// ISP routers often de-prioritise or drop pings aimed at their own interface;
// picking a silent hop would fill the record with meaningless failures.
func (c *Collector) pickResponsiveHop(ctx context.Context, candidates []string) string {
	for _, addr := range candidates {
		res := c.prober.Probe(ctx, []string{addr}, 3, c.cfg.Timeout)
		if r := res[addr]; r.OK() {
			c.log.Printf("ISP edge target: %s (rtt %v, loss %.0f%%)", addr, r.RTT, r.LossPct())
			return addr
		}
		c.log.Printf("hop %s does not answer ICMP; trying next", addr)
	}
	if len(candidates) > 0 {
		c.log.Printf("warn: no ISP hop answered ICMP; using %s as a corroborating signal only", candidates[0])
		return candidates[0]
	}
	return ""
}

func (c *Collector) registerTargets() error {
	c.targets = nil
	if c.gateway != "" {
		c.targets = append(c.targets, config.Target{Name: "gateway", Addr: c.gateway, Tier: config.TierGateway})
	}
	if c.ispEdge != "" {
		c.targets = append(c.targets, config.Target{Name: "isp_edge", Addr: c.ispEdge, Tier: config.TierISPEdge})
	}
	c.targets = append(c.targets, c.cfg.InternetAnchors()...)

	for _, t := range c.targets {
		id, err := c.st.UpsertTarget(t.Name, t.Addr, string(t.Tier))
		if err != nil {
			return fmt.Errorf("register target %s: %w", t.Name, err)
		}
		c.targetIDs[t.Name] = id
	}

	_ = c.st.SetMeta("gateway", c.gateway)
	_ = c.st.SetMeta("isp_edge", c.ispEdge)
	_ = c.st.SetMeta("interface", c.iface)
	return nil
}

// reconcileCrash closes an outage left open by a crash or kill. The end time is
// the last cycle actually observed — anything later would be invention, since
// we genuinely do not know what happened while we were not running.
func (c *Collector) reconcileCrash() {
	id, started, class, ok := c.st.UnfinishedOutage()
	if !ok {
		return
	}
	var lastTS int64
	if err := c.st.DB().QueryRow(`SELECT COALESCE(MAX(ts), 0) FROM cycles`).Scan(&lastTS); err != nil || lastTS == 0 {
		lastTS = started.Unix()
	}
	if err := c.st.CloseOutage(id, time.Unix(lastTS, 0), class); err != nil {
		c.log.Printf("warn: could not close crashed outage %d: %v", id, err)
		return
	}
	c.log.Printf("closed outage #%d (%s) left open by a previous run, at last observed cycle %s",
		id, class, time.Unix(lastTS, 0).Format(time.RFC3339))
}

// discoverWithRetry keeps trying topology discovery until it succeeds or ctx
// ends.
//
// At boot the collector can start before the network is configured, and
// `route -n get default` then reports no gateway. Treating that as fatal makes
// the process exit, which under KeepAlive becomes a crash-loop against
// launchd's restart throttle rather than a short wait. Waiting is both simpler
// and more honest: there is genuinely nothing to measure until a route exists.
//
// The window before discovery succeeds is left as a gap in the heartbeat, so it
// reduces coverage rather than being recorded as downtime — we cannot claim the
// link was down when we were not yet able to look.
func (c *Collector) discoverWithRetry(ctx context.Context) error {
	const maxBackoff = 30 * time.Second
	backoff := 2 * time.Second

	for attempt := 1; ; attempt++ {
		err := c.Discover(ctx)
		if err == nil {
			if attempt > 1 {
				c.log.Printf("network ready after %d attempts", attempt)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Printf("waiting for the network (attempt %d): %v", attempt, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// Run drives the probe loop until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) error {
	if err := c.discoverWithRetry(ctx); err != nil {
		// The only error here is cancellation, which is a clean shutdown.
		return nil
	}
	c.reconcileCrash()

	c.log.Printf("collecting every %v: gateway=%s isp_edge=%s iface=%s ssid=%q",
		c.cfg.Interval, c.gateway, c.ispEdge, c.iface, c.ssid)

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	// Housekeeping is deliberately infrequent — it touches the whole table.
	maintain := time.NewTicker(1 * time.Hour)
	defer maintain.Stop()

	// The interface and SSID shell out, so they are refreshed on their own slow
	// cadence rather than once per probe cycle.
	refresh := time.NewTicker(1 * time.Minute)
	defer refresh.Stop()

	c.cycle(ctx)
	for {
		select {
		case <-ctx.Done():
			c.log.Printf("shutting down")
			return nil
		case <-ticker.C:
			c.cycle(ctx)
		case <-refresh.C:
			c.refreshLink(ctx)
		case <-maintain.C:
			c.maintain()
		}
	}
}

func (c *Collector) refreshLink(ctx context.Context) {
	route, err := discover.DefaultRoute(ctx)
	if err != nil {
		return
	}
	ssid := discover.SSID(ctx, route.Interface)
	if route.Interface != c.iface || ssid != c.ssid || route.Gateway != c.gateway {
		c.log.Printf("link changed: iface %s->%s gateway %s->%s ssid %q->%q; rediscovering",
			c.iface, route.Interface, c.gateway, route.Gateway, c.ssid, ssid)
		if err := c.Discover(ctx); err != nil {
			c.log.Printf("warn: rediscovery failed: %v", err)
		}
	}
}

func (c *Collector) maintain() {
	if n, err := c.st.Prune(c.cfg.RetainSamples); err != nil {
		c.log.Printf("warn: prune: %v", err)
	} else if n > 0 {
		c.log.Printf("pruned %d raw sample rows past the %v retention window", n, c.cfg.RetainSamples)
	}
	if err := c.st.Rollup(c.cfg.Interval, 3); err != nil {
		c.log.Printf("warn: rollup: %v", err)
	}
}

// cycle runs one probe round and persists it.
func (c *Collector) cycle(ctx context.Context) {
	ts := time.Now().Truncate(time.Second)

	addrs := make([]string, 0, len(c.targets))
	for _, t := range c.targets {
		addrs = append(addrs, t.Addr)
	}
	results := c.prober.Probe(ctx, addrs, c.cfg.Count, c.cfg.Timeout)

	in := classify.Input{
		// A missing gateway target means discovery failed outright; treat the
		// local network as suspect rather than silently blaming the provider.
		GatewayOK: false,
		// With no ISP edge target, do not let its absence imply a failure.
		ISPEdgeOK: true,
	}

	samples := make([]store.Sample, 0, len(c.targets))
	for _, t := range c.targets {
		r := results[t.Addr]
		samples = append(samples, store.Sample{
			TargetID: c.targetIDs[t.Name],
			OK:       r.OK(),
			RTT:      r.RTT,
			LossPct:  r.LossPct(),
		})

		switch t.Tier {
		case config.TierGateway:
			in.GatewayOK = r.OK()
		case config.TierISPEdge:
			in.ISPEdgeOK = r.OK()
		case config.TierInternet:
			in.InternetTotal++
			if r.OK() {
				in.InternetUp++
			}
		}
	}

	verdict := classify.Classify(in)
	if err := c.st.WriteCycle(ts, string(verdict), c.iface, c.ssid, samples); err != nil {
		c.log.Printf("warn: write cycle: %v", err)
	}

	c.handleEvent(c.track.Observe(ts, verdict))
}

func (c *Collector) handleEvent(ev *classify.Event) {
	if ev == nil {
		return
	}
	switch {
	case ev.Opened:
		id, err := c.st.OpenOutage(ev.Start, string(ev.Class), fmt.Sprintf("gateway=%s isp_edge=%s", c.gateway, c.ispEdge))
		if err != nil {
			c.log.Printf("warn: open outage: %v", err)
			return
		}
		c.openOutageID = id
		c.log.Printf("OUTAGE START %s at %s", ev.Class, ev.Start.Format(time.RFC3339))
	case ev.Closed:
		if c.openOutageID == 0 {
			return
		}
		if err := c.st.CloseOutage(c.openOutageID, ev.End, string(ev.Class)); err != nil {
			c.log.Printf("warn: close outage: %v", err)
			return
		}
		c.log.Printf("OUTAGE END   %s lasted %v (ended %s)",
			ev.Class, ev.End.Sub(ev.Start), ev.End.Format(time.RFC3339))
		c.openOutageID = 0
	}
}
