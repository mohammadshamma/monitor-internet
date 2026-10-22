package report

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/store"
)

func TestTargetsSeriesDoesNotDropTailWhenSpanNotDivisibleByBuckets(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	id, err := st.UpsertTarget("cloudflare", "1.1.1.1", "internet")
	if err != nil {
		t.Fatalf("upsert target: %v", err)
	}

	// span = 359s with 120 buckets: integer division 359/120 truncates widthSec
	// to 2s, covering only 240s and dropping the last 119s of samples.
	since := time.Unix(1_700_000_000, 0)
	until := since.Add(359 * time.Second)

	const totalSamples = 72
	for i := 0; i < totalSamples; i++ {
		ts := since.Add(time.Duration(i*5) * time.Second) // 0s .. 355s
		if err := st.WriteCycle(ts, "OK", "en0", "", []store.Sample{
			{TargetID: id, OK: true, RTT: 10 * time.Millisecond, LossPct: 0},
		}); err != nil {
			t.Fatalf("write cycle %d: %v", i, err)
		}
	}

	series, err := TargetsSeries(st, since, until, 120, 5*time.Second)
	if err != nil {
		t.Fatalf("TargetsSeries: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	if series[0].Samples != totalSamples {
		t.Fatalf("series.Samples = %d, want %d", series[0].Samples, totalSamples)
	}

	var bucketed int
	for _, p := range series[0].Points {
		bucketed += p.Samples
	}
	if bucketed != totalSamples {
		t.Errorf("bucketed samples across Points = %d, want %d (dropped %d tail samples)",
			bucketed, totalSamples, totalSamples-bucketed)
	}
}
