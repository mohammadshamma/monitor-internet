// Command mon monitors internet connection health and attributes outages to
// the responsible network segment.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/agent"
	"github.com/mohammadshamma/monitor-internet/internal/collector"
	"github.com/mohammadshamma/monitor-internet/internal/config"
	"github.com/mohammadshamma/monitor-internet/internal/probe"
	"github.com/mohammadshamma/monitor-internet/internal/report"
	"github.com/mohammadshamma/monitor-internet/internal/store"
	"github.com/mohammadshamma/monitor-internet/internal/web"
)

const usage = `mon — internet connection health monitor

Usage:
  mon start                    run the collector (foreground; launchd runs it this way)
  mon stop                     signal a running collector to exit
  mon status                   show the current live state
  mon calibrate                measure baseline loss per target and pick the ISP hop
  mon report [flags]           summarise uptime, outages and blame
  mon serve [flags]            serve the dashboard
  mon install-agent            install and load both launchd agents
  mon uninstall-agent          unload and remove them
  mon restart-agent            restart both agents (after a rebuild)
  mon config [show|init|path]  show effective config, write an example, or print its path

Report flags:
  --since 30d                  window (30d, 24h, 90m)
  --json                       emit JSON instead of text
  --csv                        emit the outage list as CSV

Serve flags:
  --host <addr>                bind address (default from config)
  --port <n>                   bind port (default from config)

Machine-specific settings live in a config file; see: mon config path
`

func main() {
	log.SetFlags(0)

	// Fail clearly rather than with a confusing "no such file" from a shelled-out
	// macOS binary several layers down.
	if !supportedPlatform {
		fmt.Fprintln(os.Stderr,
			"mon: this tool supports macOS only — it uses launchd agents and macOS\n"+
				"     network utilities (/sbin/route, /usr/sbin/traceroute) for topology\n"+
				"     discovery. See https://github.com/mohammadshamma/monitor-internet")
		os.Exit(1)
	}

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "start":
		err = cmdStart(os.Args[2:])
	case "stop":
		err = cmdStop()
	case "status":
		err = cmdStatus()
	case "calibrate":
		err = cmdCalibrate()
	case "report":
		err = cmdReport(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "install-agent":
		err = withConfig(agent.Install)
	case "uninstall-agent":
		err = withConfig(agent.Uninstall)
	case "restart-agent":
		err = withConfig(agent.Restart)
	case "config":
		err = cmdConfig(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		log.Fatalf("mon %s: %v", os.Args[1], err)
	}
}

// withConfig runs an action against the loaded configuration.
func withConfig(fn func(config.Config) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return fn(cfg)
}

func cmdConfig(args []string) error {
	action := "show"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "path":
		fmt.Println(config.Path())
		return nil
	case "init":
		path, err := config.WriteExample()
		if err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
		return nil
	case "show":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if _, statErr := os.Stat(config.Path()); os.IsNotExist(statErr) {
			fmt.Printf("# no config file at %s — showing built-in defaults\n", config.Path())
			fmt.Printf("# run `mon config init` to create one\n")
		} else {
			fmt.Printf("# %s\n", config.Path())
		}
		fmt.Printf("interval         %v\n", cfg.Interval)
		fmt.Printf("timeout          %v\n", cfg.Timeout)
		fmt.Printf("count            %d\n", cfg.Count)
		fmt.Printf("fail/clear       %d / %d cycles\n", cfg.FailThreshold, cfg.ClearThreshold)
		if cfg.RetainSamples == 0 {
			fmt.Printf("retain samples   forever\n")
		} else {
			fmt.Printf("retain samples   %v\n", cfg.RetainSamples)
		}
		fmt.Printf("trace target     %s\n", cfg.TraceTarget)
		for _, a := range cfg.Anchors {
			fmt.Printf("anchor           %-12s %s\n", a.Name, a.Addr)
		}
		fmt.Printf("gateway          %s\n", orAuto(cfg.Gateway))
		fmt.Printf("isp edge         %s\n", orAuto(cfg.ISPEdge))
		fmt.Printf("dashboard        %s:%d\n", cfg.Web.Host, cfg.Web.Port)
		fmt.Printf("launchd labels   %s, %s\n", cfg.CollectorLabel(), cfg.WebLabel())
		fmt.Printf("database         %s\n", config.DBPath())
		return nil
	default:
		return fmt.Errorf("unknown config action %q (use show, init, or path)", action)
	}
}

func orAuto(s string) string {
	if s == "" {
		return "(auto-discovered)"
	}
	return s
}

// openStore opens the database read-write, creating directories as needed.
func openStore() (*store.Store, error) {
	if err := config.EnsureDirs(); err != nil {
		return nil, err
	}
	return store.Open(config.DBPath())
}

// openStoreRO opens the database read-only for reporting and serving.
func openStoreRO() (*store.Store, error) {
	if _, err := os.Stat(config.DBPath()); os.IsNotExist(err) {
		return nil, fmt.Errorf("no database yet at %s — run `mon start` first", config.DBPath())
	}
	return store.OpenReadOnly(config.DBPath())
}

func cmdStart(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("start", flag.ExitOnError)
	interval := fs.Duration("interval", cfg.Interval, "probe interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Interval = *interval

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	if err := writePid(); err != nil {
		return err
	}
	defer os.Remove(config.PidPath())

	lg := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	c, err := collector.New(cfg, st, lg)
	if err != nil {
		return err
	}
	defer c.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return c.Run(ctx)
}

func writePid() error {
	return os.WriteFile(config.PidPath(), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func cmdStop() error {
	data, err := os.ReadFile(config.PidPath())
	if err != nil {
		return fmt.Errorf("no pidfile at %s; is the collector running?", config.PidPath())
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("bad pidfile: %w", err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	fmt.Printf("sent SIGTERM to collector (pid %d)\n", pid)
	fmt.Println("note: if it is running under launchd, KeepAlive will restart it.")
	fmt.Println("      use `mon uninstall-agent` to stop it for real.")
	return nil
}

func cmdStatus() error {
	st, err := openStoreRO()
	if err != nil {
		return err
	}
	defer st.Close()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	s, err := report.CurrentStatus(st, cfg.Interval)
	if err != nil {
		return err
	}

	fmt.Printf("verdict     %s\n", s.Verdict)
	if s.LastCycle.IsZero() {
		fmt.Println("last cycle  never")
	} else {
		fmt.Printf("last cycle  %s (%s ago)\n",
			s.LastCycle.Format(time.RFC3339), time.Since(s.LastCycle).Truncate(time.Second))
	}
	if s.Stale {
		fmt.Println("            ⚠ stale — the collector does not appear to be running")
	}
	fmt.Printf("gateway     %s\n", s.Gateway)
	fmt.Printf("isp edge    %s\n", s.ISPEdge)
	fmt.Printf("interface   %s\n", s.Interface)
	return nil
}

// cmdCalibrate measures how each target behaves when the network is healthy.
// The point is to catch routers that de-prioritise ICMP before their silence is
// mistaken for an outage.
func cmdCalibrate() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	lg := log.New(os.Stdout, "", 0)
	c, err := collector.New(cfg, st, lg)
	if err != nil {
		return err
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fmt.Println("discovering topology…")
	if err := c.Discover(ctx); err != nil {
		return err
	}

	targets := c.Targets()
	addrs := make([]string, 0, len(targets))
	for _, t := range targets {
		addrs = append(addrs, t.Addr)
	}

	const rounds = 20
	fmt.Printf("\nsampling %d targets over %d rounds…\n\n", len(addrs), rounds)

	p, err := probe.New()
	if err != nil {
		return err
	}
	defer p.Close()

	sent := map[string]int{}
	recv := map[string]int{}
	best := map[string]time.Duration{}

	for i := 0; i < rounds; i++ {
		res := p.Probe(ctx, addrs, 1, 2*time.Second)
		for a, r := range res {
			sent[a] += r.Sent
			recv[a] += r.Recv
			if r.OK() && (best[a] == 0 || r.RTT < best[a]) {
				best[a] = r.RTT
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	fmt.Printf("%-12s %-16s %-10s %8s %10s\n", "TIER", "ADDRESS", "NAME", "LOSS", "BEST RTT")
	fmt.Println(strings.Repeat("─", 62))
	for _, t := range targets {
		loss := 0.0
		if sent[t.Addr] > 0 {
			loss = float64(sent[t.Addr]-recv[t.Addr]) / float64(sent[t.Addr]) * 100
		}
		if err := st.SetBaselineLoss(t.Name, loss); err != nil {
			return err
		}
		fmt.Printf("%-12s %-16s %-10s %7.1f%% %10v\n", t.Tier, t.Addr, t.Name, loss, best[t.Addr].Truncate(time.Microsecond))
	}

	fmt.Println()
	for _, t := range targets {
		if sent[t.Addr] == 0 {
			continue
		}
		loss := float64(sent[t.Addr]-recv[t.Addr]) / float64(sent[t.Addr]) * 100
		if loss > 20 && t.Tier == config.TierISPEdge {
			fmt.Printf("⚠ %s drops %.0f%% of pings while idle. It is treated as a corroborating\n", t.Addr, loss)
			fmt.Printf("  signal only, so this will not create false outages.\n")
		}
	}
	return nil
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	since := fs.String("since", "30d", "window: 30d, 24h, 90m")
	asJSON := fs.Bool("json", false, "emit JSON")
	asCSV := fs.Bool("csv", false, "emit the outage list as CSV")
	if err := fs.Parse(args); err != nil {
		return err
	}

	window, err := report.ParseSince(*since)
	if err != nil {
		return err
	}

	st, err := openStoreRO()
	if err != nil {
		return err
	}
	defer st.Close()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	until := time.Now()
	from := until.Add(-window)
	interval := cfg.Interval

	summary, err := report.BuildSummary(st, from, until, interval)
	if err != nil {
		return err
	}
	outages, err := report.Outages(st, from, until)
	if err != nil {
		return err
	}

	switch {
	case *asCSV:
		return report.WriteCSV(os.Stdout, outages)
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]interface{}{
			"summary": summary,
			"outages": outages,
		})
	}

	hist, err := report.HourHistogram(st, from, until, interval)
	if err != nil {
		return err
	}
	links, err := report.Links(st, from, until)
	if err != nil {
		return err
	}
	report.WriteText(os.Stdout, summary, outages, hist, links)
	return nil
}

func cmdServe(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	host := fs.String("host", cfg.Web.Host, "bind address")
	port := fs.Int("port", cfg.Web.Port, "bind port")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStoreRO()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := web.New(st, cfg.Interval, log.New(os.Stdout, "", log.LstdFlags|log.LUTC))
	return srv.Run(ctx, *host, *port)
}
