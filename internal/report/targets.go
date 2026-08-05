package report

import (
	"fmt"
	"math"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/store"
)

// TargetPoint is one bucket of a target's latency series.
type TargetPoint struct {
	Start time.Time `json:"start"`
	// AvgRTTms is the mean round trip in the bucket; null when nothing answered.
	AvgRTTms *float64 `json:"avg_rtt_ms"`
	// MaxRTTms exposes spikes that the mean would hide.
	MaxRTTms *float64 `json:"max_rtt_ms"`
	LossPct  float64  `json:"loss_pct"`
	Samples  int      `json:"samples"`
}

// TargetSeries is one probe target's latency history plus summary statistics.
type TargetSeries struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
	Tier string `json:"tier"`

	Points []TargetPoint `json:"points"`

	// Summary statistics over the whole window.
	Samples   int      `json:"samples"`
	LossPct   float64  `json:"loss_pct"`
	MinRTTms  *float64 `json:"min_rtt_ms"`
	AvgRTTms  *float64 `json:"avg_rtt_ms"`
	P95RTTms  *float64 `json:"p95_rtt_ms"`
	MaxRTTms  *float64 `json:"max_rtt_ms"`
	Stride    int64    `json:"stride_seconds"`
	Truncated bool     `json:"truncated"`
}

// maxScanRows bounds how many raw sample rows a single chart query may touch.
// Samples are now kept forever, so without a bound these queries would get
// slower every day the tool runs. Past the bound the query strides over the
// data instead of reading all of it — the shape of the series is preserved and
// the cost stops growing.
const maxScanRows = 400_000

// strideFor returns the sampling stride in seconds, and whether striding is in
// effect. A stride of 0 means "read everything".
func strideFor(since, until time.Time, interval time.Duration, targets int) (int64, bool) {
	if targets < 1 {
		targets = 1
	}
	step := int64(interval.Seconds())
	if step < 1 {
		step = 1
	}
	window := int64(until.Sub(since).Seconds())
	estimated := (window / step) * int64(targets)
	if estimated <= maxScanRows {
		return 0, false
	}
	factor := float64(estimated) / float64(maxScanRows)
	stride := int64(math.Ceil(factor)) * step
	return stride, true
}

// TargetsSeries returns per-target latency and loss over the window.
func TargetsSeries(st *store.Store, since, until time.Time, buckets int, interval time.Duration) ([]TargetSeries, error) {
	if buckets < 1 {
		buckets = 120
	}
	span := until.Sub(since)
	if span <= 0 {
		return nil, nil
	}

	type target struct {
		id   int64
		name string
		addr string
		tier string
	}
	rows, err := st.DB().Query(`SELECT id, name, addr, tier FROM targets ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query targets: %w", err)
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.name, &t.addr, &t.tier); err != nil {
			rows.Close()
			return nil, err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}

	stride, truncated := strideFor(since, until, interval, len(targets))
	// A stride clause of "" reads every row; otherwise keep one cycle per
	// stride window. Cycle timestamps are not perfectly aligned, so this is
	// approximate by design — it thins the series without biasing it.
	strideClause := ""
	if stride > 0 {
		strideClause = fmt.Sprintf(" AND (ts %% %d) < %d", stride, int64(interval.Seconds()))
	}

	widthSec := int64(span.Seconds()) / int64(buckets)
	if widthSec < 1 {
		widthSec = 1
		buckets = int(span.Seconds())
	}

	out := make([]TargetSeries, 0, len(targets))
	for _, t := range targets {
		series := TargetSeries{
			Name: t.name, Addr: t.addr, Tier: t.tier,
			Stride: stride, Truncated: truncated,
			Points: make([]TargetPoint, buckets),
		}
		for i := range series.Points {
			series.Points[i].Start = since.Add(time.Duration(i*int(widthSec)) * time.Second)
		}

		q := fmt.Sprintf(`
			SELECT (ts - ?) / ?, AVG(rtt_us), MAX(rtt_us), AVG(loss_pct), COUNT(*)
			FROM samples
			WHERE target_id = ? AND ts >= ? AND ts <= ?%s
			GROUP BY 1`, strideClause)

		bRows, err := st.DB().Query(q, since.Unix(), widthSec, t.id, since.Unix(), until.Unix())
		if err != nil {
			return nil, fmt.Errorf("query samples for %s: %w", t.name, err)
		}
		for bRows.Next() {
			var idx int
			var avgUS, maxUS, avgLoss *float64
			var count int
			if err := bRows.Scan(&idx, &avgUS, &maxUS, &avgLoss, &count); err != nil {
				bRows.Close()
				return nil, err
			}
			if idx < 0 || idx >= buckets {
				continue
			}
			p := &series.Points[idx]
			p.Samples = count
			if avgUS != nil {
				v := *avgUS / 1000
				p.AvgRTTms = &v
			}
			if maxUS != nil {
				v := *maxUS / 1000
				p.MaxRTTms = &v
			}
			if avgLoss != nil {
				p.LossPct = *avgLoss
			}
		}
		bRows.Close()
		if err := bRows.Err(); err != nil {
			return nil, err
		}

		if err := targetStats(st, t.id, since, until, strideClause, &series); err != nil {
			return nil, err
		}
		out = append(out, series)
	}
	return out, nil
}

// targetStats fills in the whole-window summary for one target.
func targetStats(st *store.Store, id int64, since, until time.Time, strideClause string, s *TargetSeries) error {
	q := fmt.Sprintf(`
		SELECT COUNT(*), AVG(loss_pct), MIN(rtt_us), AVG(rtt_us), MAX(rtt_us)
		FROM samples WHERE target_id = ? AND ts >= ? AND ts <= ?%s`, strideClause)

	var count int
	var avgLoss, minUS, avgUS, maxUS *float64
	if err := st.DB().QueryRow(q, id, since.Unix(), until.Unix()).
		Scan(&count, &avgLoss, &minUS, &avgUS, &maxUS); err != nil {
		return fmt.Errorf("target stats: %w", err)
	}
	s.Samples = count
	if avgLoss != nil {
		s.LossPct = *avgLoss
	}
	s.MinRTTms = msPtr(minUS)
	s.AvgRTTms = msPtr(avgUS)
	s.MaxRTTms = msPtr(maxUS)

	// p95 by offset, which is exact rather than interpolated and costs one
	// indexed scan. Only answered probes have an RTT, so nulls are excluded.
	cq := fmt.Sprintf(`SELECT COUNT(*) FROM samples
		WHERE target_id = ? AND ts >= ? AND ts <= ? AND rtt_us IS NOT NULL%s`, strideClause)
	var answered int
	if err := st.DB().QueryRow(cq, id, since.Unix(), until.Unix()).Scan(&answered); err != nil {
		return err
	}
	if answered > 0 {
		offset := int(float64(answered) * 0.95)
		if offset >= answered {
			offset = answered - 1
		}
		pq := fmt.Sprintf(`SELECT rtt_us FROM samples
			WHERE target_id = ? AND ts >= ? AND ts <= ? AND rtt_us IS NOT NULL%s
			ORDER BY rtt_us LIMIT 1 OFFSET ?`, strideClause)
		var p95 float64
		if err := st.DB().QueryRow(pq, id, since.Unix(), until.Unix(), offset).Scan(&p95); err == nil {
			v := p95 / 1000
			s.P95RTTms = &v
		}
	}
	return nil
}

func msPtr(us *float64) *float64 {
	if us == nil {
		return nil
	}
	v := *us / 1000
	return &v
}
