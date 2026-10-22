package report

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/classify"
	"github.com/mohammadshamma/monitor-internet/internal/store"
)

const interval = 5 * time.Second

// fixture builds a real SQLite database with a controlled cycle history.
func fixture(t *testing.T, base time.Time, verdicts []classify.Verdict) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id, err := st.UpsertTarget("gateway", "198.51.100.1", "gateway")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	tr := classify.NewTracker(3, 3)
	var openID int64
	for i, v := range verdicts {
		ts := base.Add(time.Duration(i) * interval)
		err := st.WriteCycle(ts, string(v), "iface0", "", []store.Sample{
			{TargetID: id, OK: v != classify.LANFault, RTT: time.Millisecond, LossPct: 0},
		})
		if err != nil {
			t.Fatalf("write cycle %d: %v", i, err)
		}
		if ev := tr.Observe(ts, v); ev != nil {
			switch {
			case ev.Opened:
				openID, err = st.OpenOutage(ev.Start, string(ev.Class), "")
				if err != nil {
					t.Fatalf("open outage: %v", err)
				}
			case ev.Closed:
				if err := st.CloseOutage(openID, ev.End, string(ev.Class)); err != nil {
					t.Fatalf("close outage: %v", err)
				}
				openID = 0
			}
		}
	}
	return st
}

func repeat(v classify.Verdict, n int) []classify.Verdict {
	out := make([]classify.Verdict, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestSummaryPerfectUptime(t *testing.T) {
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	// 720 cycles at 5s = exactly one hour.
	st := fixture(t, base, repeat(classify.OK, 720))

	s, err := BuildSummary(st, base, base.Add(time.Hour), interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if s.ISPAvailabilityPct != 100 {
		t.Errorf("ISP availability = %.4f, want 100", s.ISPAvailabilityPct)
	}
	if s.ISPDowntimeSeconds != 0 {
		t.Errorf("ISP downtime = %d, want 0", s.ISPDowntimeSeconds)
	}
	if s.CoveragePct < 99 {
		t.Errorf("coverage = %.2f%%, want ~100%%", s.CoveragePct)
	}
}

// The single most important accounting rule: a local fault is downtime, but it
// is not the provider's downtime.
func TestLANFaultNeverCountsAgainstTheISP(t *testing.T) {
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	verdicts := append(repeat(classify.OK, 600), repeat(classify.LANFault, 120)...)
	st := fixture(t, base, verdicts)

	s, err := BuildSummary(st, base, base.Add(time.Hour), interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}

	if s.ISPDowntimeSeconds != 0 {
		t.Errorf("ISP downtime = %ds, want 0: LAN faults must not be blamed on the provider",
			s.ISPDowntimeSeconds)
	}
	if s.ISPAvailabilityPct != 100 {
		t.Errorf("ISP availability = %.4f%%, want 100%%", s.ISPAvailabilityPct)
	}
	// 120 cycles * 5s = 600s of local downtime, which must still be reported.
	if s.LANDowntimeSeconds != 600 {
		t.Errorf("LAN downtime = %ds, want 600", s.LANDowntimeSeconds)
	}
	if s.TotalAvailabilityPct >= 100 {
		t.Errorf("total availability = %.4f%%, should be below 100%% given local downtime",
			s.TotalAvailabilityPct)
	}
}

func TestISPDowntimeIsCounted(t *testing.T) {
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	// 60 cycles * 5s = 300s = 5 minutes of provider downtime out of an hour.
	verdicts := append(repeat(classify.OK, 660), repeat(classify.ISPFault, 60)...)
	st := fixture(t, base, verdicts)

	s, err := BuildSummary(st, base, base.Add(time.Hour), interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if s.ISPDowntimeSeconds != 300 {
		t.Errorf("ISP downtime = %ds, want 300", s.ISPDowntimeSeconds)
	}
	want := float64(3600-300) / 3600 * 100
	if diff := s.ISPAvailabilityPct - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("ISP availability = %.4f%%, want %.4f%%", s.ISPAvailabilityPct, want)
	}
}

// A gap in the heartbeat means the monitor was not running. That time must
// reduce coverage, not be silently counted as either uptime or downtime.
func TestHeartbeatGapReducesCoverageNotAvailability(t *testing.T) {
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	// Only the first half hour is monitored; the rest of the window is a gap.
	st := fixture(t, base, repeat(classify.OK, 360))

	s, err := BuildSummary(st, base, base.Add(time.Hour), interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}

	if s.ISPAvailabilityPct != 100 {
		t.Errorf("ISP availability = %.4f%%, want 100%%: a gap is not downtime",
			s.ISPAvailabilityPct)
	}
	if s.ISPDowntimeSeconds != 0 {
		t.Errorf("ISP downtime = %ds, want 0: an unmonitored gap is not an outage",
			s.ISPDowntimeSeconds)
	}
	if s.CoveragePct > 55 || s.CoveragePct < 45 {
		t.Errorf("coverage = %.2f%%, want ~50%%: the gap must show up here", s.CoveragePct)
	}
}

func TestDebouncedBlipRecordsZeroDowntime(t *testing.T) {
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	verdicts := repeat(classify.OK, 720)
	// Sub-threshold blips (1 ISP_FAULT, 2 ISP_UPSTREAM, 1 LAN_FAULT) never open an outage.
	verdicts[10] = classify.ISPFault
	verdicts[50] = classify.ISPUpstream
	verdicts[51] = classify.ISPUpstream
	verdicts[100] = classify.LANFault
	st := fixture(t, base, verdicts)

	until := base.Add(time.Hour)
	s, err := BuildSummary(st, base, until, interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if s.OutageCount != 0 {
		t.Fatalf("OutageCount = %d, want 0", s.OutageCount)
	}
	if s.ISPDowntimeSeconds != 0 {
		t.Errorf("ISPDowntimeSeconds = %ds, want 0 for debounced blips", s.ISPDowntimeSeconds)
	}
	if s.LANDowntimeSeconds != 0 {
		t.Errorf("LANDowntimeSeconds = %ds, want 0 for debounced blips", s.LANDowntimeSeconds)
	}
	if s.ISPAvailabilityPct != 100 {
		t.Errorf("ISPAvailabilityPct = %.4f%%, want 100%%", s.ISPAvailabilityPct)
	}

	hist, err := HourHistogram(st, base, until, interval)
	if err != nil {
		t.Fatalf("HourHistogram: %v", err)
	}
	for h, sec := range hist {
		if sec != 0 {
			t.Errorf("hist[%d] = %ds, want 0 for debounced blips", h, sec)
		}
	}
}

func TestEscalatedOutageAttributesFullDurationToISP(t *testing.T) {
	base := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	verdicts := repeat(classify.OK, 720)
	// 2 LAN_FAULT cycles followed by 2 ISP_FAULT cycles (4 cycles = 20s) escalate
	// the entire outage session to ISP_FAULT on close.
	verdicts[10] = classify.LANFault
	verdicts[11] = classify.LANFault
	verdicts[12] = classify.ISPFault
	verdicts[13] = classify.ISPFault
	st := fixture(t, base, verdicts)

	until := base.Add(time.Hour)
	s, err := BuildSummary(st, base, until, interval)
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if s.OutageCount != 1 || s.ISPOutageCount != 1 {
		t.Fatalf("outages = %d (isp %d), want 1 (isp 1)", s.OutageCount, s.ISPOutageCount)
	}
	if s.ISPDowntimeSeconds != 20 {
		t.Errorf("ISPDowntimeSeconds = %ds, want 20 (all 4 cycles of escalated outage)", s.ISPDowntimeSeconds)
	}
	if s.LANDowntimeSeconds != 0 {
		t.Errorf("LANDowntimeSeconds = %ds, want 0 after escalation to ISP_FAULT", s.LANDowntimeSeconds)
	}
	if s.LongestOutageSeconds != 20 {
		t.Errorf("LongestOutageSeconds = %ds, want 20", s.LongestOutageSeconds)
	}

	hist, err := HourHistogram(st, base, until, interval)
	if err != nil {
		t.Fatalf("HourHistogram: %v", err)
	}
	var histTotal int64
	for _, sec := range hist {
		histTotal += sec
	}
	if histTotal != 20 {
		t.Errorf("HourHistogram total = %ds, want 20", histTotal)
	}
}

func TestDominantPicksWorstNotMostFrequent(t *testing.T) {
	// 99 good cycles and one provider failure: the bucket must show the failure.
	got := dominant(map[string]int{
		string(classify.OK):       99,
		string(classify.ISPFault): 1,
	})
	if got != string(classify.ISPFault) {
		t.Errorf("dominant = %q, want %q: a brief outage must not be averaged away",
			got, classify.ISPFault)
	}
}

func TestParseSince(t *testing.T) {
	tests := map[string]time.Duration{
		"30d": 30 * 24 * time.Hour,
		"24h": 24 * time.Hour,
		"90m": 90 * time.Minute,
	}
	for in, want := range tests {
		got, err := ParseSince(in)
		if err != nil {
			t.Errorf("ParseSince(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSince(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseSince("banana"); err == nil {
		t.Error("ParseSince(\"banana\") should fail")
	}
}

func TestHumanDuration(t *testing.T) {
	tests := map[int64]string{
		0: "0s", 45: "45s", 90: "1m 30s", 3660: "1h 1m", 90000: "1d 1h",
	}
	for in, want := range tests {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%d) = %q, want %q", in, got, want)
		}
	}
}
