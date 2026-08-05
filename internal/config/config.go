// Package config holds runtime configuration and the well-known filesystem
// locations the tool uses.
//
// Everything machine-specific lives in a config file outside the repository, so
// the code carries no knowledge of any particular network, host, or account.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Tier identifies which segment of the network path a target proves something
// about. The whole design rests on this distinction: a failure is only useful
// evidence if you know which tier it happened in.
type Tier string

const (
	// TierGateway is the local router. A failure here is the user's own fault.
	TierGateway Tier = "gateway"
	// TierISPEdge is the first hop beyond the router, inside the ISP's network.
	TierISPEdge Tier = "isp_edge"
	// TierInternet are well-known anchors out on the public internet.
	TierInternet Tier = "internet"
)

// DefaultLabelPrefix is the launchd label namespace. Override it in the config
// file if you prefer your own reverse-DNS namespace.
const DefaultLabelPrefix = "net.monitorinternet"

// Target is a single probe destination.
type Target struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
	Tier Tier   `json:"-"`
}

// Web holds dashboard bind settings.
type Web struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Config is the full runtime configuration.
type Config struct {
	// Interval between probe cycles.
	Interval time.Duration
	// Timeout for a single ICMP echo to come back.
	Timeout time.Duration
	// Count is how many echoes are sent per target per cycle.
	Count int
	// FailThreshold is how many consecutive bad cycles open an outage.
	FailThreshold int
	// ClearThreshold is how many consecutive good cycles close one.
	ClearThreshold int
	// RetainSamples is how long raw per-probe rows are kept before pruning.
	// Zero means keep them forever, which is the default: latency history is
	// only useful in hindsight, and deleting it forecloses questions that have
	// not been asked yet.
	RetainSamples time.Duration

	// TraceTarget is traced to discover the first hop past the router.
	TraceTarget string
	// Anchors are the public internet probe targets.
	Anchors []Target
	// Gateway and ISPEdge pin the topology by hand. Empty means auto-discover,
	// which is right for almost everyone; set them only for unusual networks
	// (double NAT, a router that never answers ICMP).
	Gateway string
	ISPEdge string

	Web Web
	// LabelPrefix namespaces the launchd agent labels.
	LabelPrefix string
}

// Default returns the standard configuration.
func Default() Config {
	return Config{
		Interval:       5 * time.Second,
		Timeout:        2 * time.Second,
		Count:          3,
		FailThreshold:  3,
		ClearThreshold: 3,
		RetainSamples:  0, // keep raw samples indefinitely
		TraceTarget:    "1.1.1.1",
		Anchors: []Target{
			{Name: "cloudflare", Addr: "1.1.1.1", Tier: TierInternet},
			{Name: "google", Addr: "8.8.8.8", Tier: TierInternet},
			{Name: "quad9", Addr: "9.9.9.9", Tier: TierInternet},
		},
		Web:         Web{Host: "0.0.0.0", Port: 8765},
		LabelPrefix: DefaultLabelPrefix,
	}
}

// InternetAnchors returns the configured public anchors.
//
// Three independent operators by default, so one anchor having a bad day is
// never mistaken for an outage — a quorum is required.
func (c Config) InternetAnchors() []Target {
	out := make([]Target, 0, len(c.Anchors))
	for _, a := range c.Anchors {
		a.Tier = TierInternet
		out = append(out, a)
	}
	return out
}

// CollectorLabel is the launchd label for the probe loop.
func (c Config) CollectorLabel() string { return c.LabelPrefix + ".collector" }

// WebLabel is the launchd label for the dashboard.
func (c Config) WebLabel() string { return c.LabelPrefix + ".web" }

// fileConfig mirrors Config with durations as strings, which is what a
// hand-edited config file should contain.
type fileConfig struct {
	Interval       string   `json:"interval,omitempty"`
	Timeout        string   `json:"timeout,omitempty"`
	Count          *int     `json:"count,omitempty"`
	FailThreshold  *int     `json:"fail_threshold,omitempty"`
	ClearThreshold *int     `json:"clear_threshold,omitempty"`
	RetainSamples  string   `json:"retain_samples,omitempty"`
	TraceTarget    string   `json:"trace_target,omitempty"`
	Anchors        []Target `json:"internet_anchors,omitempty"`
	Gateway        string   `json:"gateway,omitempty"`
	ISPEdge        string   `json:"isp_edge,omitempty"`
	Web            *Web     `json:"web,omitempty"`
	LabelPrefix    string   `json:"launchd_label_prefix,omitempty"`
}

// Path is where the config file lives.
func Path() string {
	if v := os.Getenv("MONITOR_INTERNET_CONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, ".config", "monitor-internet", "config.json")
}

// Load reads the config file over the defaults. A missing file is not an error
// — the defaults are a complete, working configuration.
func Load() (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(Path())
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", Path(), err)
	}

	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", Path(), err)
	}

	if err := applyDuration(fc.Interval, &cfg.Interval, "interval"); err != nil {
		return cfg, err
	}
	if err := applyDuration(fc.Timeout, &cfg.Timeout, "timeout"); err != nil {
		return cfg, err
	}
	if err := applyDuration(fc.RetainSamples, &cfg.RetainSamples, "retain_samples"); err != nil {
		return cfg, err
	}
	if fc.Count != nil {
		cfg.Count = *fc.Count
	}
	if fc.FailThreshold != nil {
		cfg.FailThreshold = *fc.FailThreshold
	}
	if fc.ClearThreshold != nil {
		cfg.ClearThreshold = *fc.ClearThreshold
	}
	if fc.TraceTarget != "" {
		cfg.TraceTarget = fc.TraceTarget
	}
	if len(fc.Anchors) > 0 {
		cfg.Anchors = fc.Anchors
	}
	if fc.Gateway != "" {
		cfg.Gateway = fc.Gateway
	}
	if fc.ISPEdge != "" {
		cfg.ISPEdge = fc.ISPEdge
	}
	if fc.Web != nil {
		if fc.Web.Host != "" {
			cfg.Web.Host = fc.Web.Host
		}
		if fc.Web.Port != 0 {
			cfg.Web.Port = fc.Web.Port
		}
	}
	if fc.LabelPrefix != "" {
		cfg.LabelPrefix = fc.LabelPrefix
	}

	return cfg, cfg.validate()
}

func applyDuration(s string, dst *time.Duration, name string) error {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("config %s: %w", name, err)
	}
	*dst = d
	return nil
}

func (c Config) validate() error {
	if c.Interval < time.Second {
		return fmt.Errorf("config interval must be at least 1s, got %v", c.Interval)
	}
	if c.Count < 1 {
		return fmt.Errorf("config count must be at least 1, got %d", c.Count)
	}
	if c.FailThreshold < 1 || c.ClearThreshold < 1 {
		return fmt.Errorf("config fail_threshold and clear_threshold must be at least 1")
	}
	if len(c.Anchors) == 0 {
		return fmt.Errorf("config internet_anchors must not be empty")
	}
	if c.Web.Port < 1 || c.Web.Port > 65535 {
		return fmt.Errorf("config web.port out of range: %d", c.Web.Port)
	}
	return nil
}

// WriteExample writes the current defaults to the config path, so there is
// something concrete to edit. It refuses to clobber an existing file.
func WriteExample() (string, error) {
	path := Path()
	if _, err := os.Stat(path); err == nil {
		return path, fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, err
	}

	d := Default()
	fc := fileConfig{
		Interval:       d.Interval.String(),
		Timeout:        d.Timeout.String(),
		Count:          &d.Count,
		FailThreshold:  &d.FailThreshold,
		ClearThreshold: &d.ClearThreshold,
		RetainSamples:  "0s",
		TraceTarget:    d.TraceTarget,
		Anchors:        d.Anchors,
		Web:            &d.Web,
		LabelPrefix:    d.LabelPrefix,
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return path, err
	}
	return path, os.WriteFile(path, append(data, '\n'), 0o644)
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// DBPath is where the SQLite database lives.
func DBPath() string {
	return filepath.Join(home(), ".local", "share", "monitor-internet", "monitor.db")
}

// StateDir holds logs and the collector pidfile.
func StateDir() string {
	return filepath.Join(home(), ".local", "state", "monitor-internet")
}

// LogDir follows the ~/.local/state/<name>/log convention.
func LogDir() string { return filepath.Join(StateDir(), "log") }

// PidPath is the collector's pidfile, used by `mon stop` and `mon status`.
func PidPath() string { return filepath.Join(StateDir(), "collector.pid") }

// EnsureDirs creates the data and state directories if they are missing.
func EnsureDirs() error {
	for _, d := range []string{filepath.Dir(DBPath()), LogDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
