package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// WriteText renders the human-readable report — the artifact to actually put in
// front of a provider.
func WriteText(w io.Writer, s *Summary, outages []Outage, hist [24]int64, links []LinkChange) {
	p := func(format string, args ...interface{}) { fmt.Fprintf(w, format+"\n", args...) }

	p("")
	p("  INTERNET CONNECTION REPORT")
	p("  %s  →  %s  (%s)",
		s.Since.Format("2006-01-02 15:04"), s.Until.Format("2006-01-02 15:04"),
		humanDuration(s.WindowSeconds))
	p("  %s", strings.Repeat("─", 64))
	p("")

	p("  ISP availability      %7.3f%%   ← the number to cite", s.ISPAvailabilityPct)
	p("  Overall availability  %7.3f%%   (also counts your own LAN faults)", s.TotalAvailabilityPct)
	p("  Coverage              %7.3f%%   (%s of %s actually monitored)",
		s.CoveragePct, humanDuration(s.MonitoredSeconds), humanDuration(s.WindowSeconds))
	p("")

	if s.CoveragePct < 95 && s.WindowSeconds >= 600 {
		p("  ⚠ Coverage is below 95%%. The monitor was not running for part of this")
		p("    window, so these figures describe only the time it was watching.")
		p("")
	}

	p("  DOWNTIME BY CAUSE")
	p("    Provider (ISP_FAULT + ISP_UPSTREAM)   %12s", humanDuration(s.ISPDowntimeSeconds))
	p("    Your own equipment (LAN_FAULT)        %12s   ← not the ISP's fault",
		humanDuration(s.LANDowntimeSeconds))
	p("")

	p("  OUTAGES")
	p("    Total                %d  (%d attributable to the provider)", s.OutageCount, s.ISPOutageCount)
	if s.LongestOutageSeconds > 0 {
		p("    Longest              %s  (%s)", humanDuration(s.LongestOutageSeconds), s.LongestOutageClass)
	}
	if s.MTBFSeconds > 0 {
		p("    Mean time between    %s", humanDuration(s.MTBFSeconds))
	}
	p("")

	if len(outages) > 0 {
		p("  %-20s %10s  %-14s %s", "STARTED", "DURATION", "CLASS", "BLAME")
		p("  %s", strings.Repeat("─", 64))
		shown := outages
		const maxRows = 40
		if len(shown) > maxRows {
			shown = shown[:maxRows]
		}
		for _, o := range shown {
			blame := "you"
			if o.BlamesISP {
				blame = "PROVIDER"
			}
			dur := humanDuration(o.Duration)
			if o.Ongoing {
				dur += " (ongoing)"
			}
			p("  %-20s %10s  %-14s %s",
				o.Started.Format("2006-01-02 15:04:05"), dur, o.Class, blame)
		}
		if len(outages) > maxRows {
			p("  … and %d more (use --json or --csv for the full list)", len(outages)-maxRows)
		}
		p("")
	}

	// Only worth drawing once there is something to see.
	var histTotal int64
	for _, v := range hist {
		histTotal += v
	}
	if histTotal > 0 {
		p("  PROVIDER DOWNTIME BY HOUR OF DAY")
		var peak int64
		for _, v := range hist {
			if v > peak {
				peak = v
			}
		}
		for h := 0; h < 24; h++ {
			if hist[h] == 0 {
				continue
			}
			bars := int(float64(hist[h]) / float64(peak) * 40)
			if bars < 1 {
				bars = 1
			}
			p("    %02d:00  %-40s %s", h, strings.Repeat("█", bars), humanDuration(hist[h]))
		}
		p("")
	}

	if len(links) > 1 {
		p("  ⚠ LINK CHANGED DURING THIS WINDOW")
		p("    Comparisons across the whole period are not like-for-like.")
		for _, l := range links {
			name := l.Interface
			if l.SSID != "" {
				name += " / " + l.SSID
			}
			p("    %-24s %s → %s", name,
				l.First.Format("01-02 15:04"), l.Last.Format("01-02 15:04"))
		}
		p("")
	}
}

// WriteCSV emits the outage list as CSV.
func WriteCSV(w io.Writer, outages []Outage) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{"started", "ended", "duration_seconds", "class", "blames_isp", "ongoing"}); err != nil {
		return err
	}
	for _, o := range outages {
		ended := ""
		if !o.Ongoing {
			ended = o.Ended.Format(time.RFC3339)
		}
		if err := cw.Write([]string{
			o.Started.Format(time.RFC3339),
			ended,
			strconv.FormatInt(o.Duration, 10),
			o.Class,
			strconv.FormatBool(o.BlamesISP),
			strconv.FormatBool(o.Ongoing),
		}); err != nil {
			return err
		}
	}
	return nil
}

// humanDuration renders seconds in the largest sensible unit.
func humanDuration(sec int64) string {
	switch {
	case sec <= 0:
		return "0s"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	case sec < 86400:
		return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
	default:
		return fmt.Sprintf("%dd %dh", sec/86400, (sec%86400)/3600)
	}
}

// ParseSince accepts durations like 30d, 24h, 90m.
func ParseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (try 30d, 24h, 90m)", s)
	}
	return d, nil
}
