// Package report turns stored cycles and outages into the statistics that
// answer the question the tool exists for: how much downtime is the provider's
// fault?
//
// Both the CLI and the dashboard call these functions, so the two can never
// disagree about the numbers.
package report

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/classify"
	"github.com/mohammadshamma/monitor-internet/internal/store"
)

// Summary is the headline result for a time window.
type Summary struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	// WindowSeconds is wall-clock length of the window.
	WindowSeconds int64 `json:"window_seconds"`
	// MonitoredSeconds is how much of it the collector was actually running.
	MonitoredSeconds int64 `json:"monitored_seconds"`
	// CoveragePct is MonitoredSeconds/WindowSeconds. Quoting an availability
	// figure without this is what makes a monitoring claim unfalsifiable, so it
	// is reported everywhere availability is.
	CoveragePct float64 `json:"coverage_pct"`

	VerdictSeconds map[string]int64 `json:"verdict_seconds"`

	// ISPDowntimeSeconds counts only ISP_FAULT and ISP_UPSTREAM.
	ISPDowntimeSeconds int64 `json:"isp_downtime_seconds"`
	// LANDowntimeSeconds is the user's own equipment, reported separately so it
	// can never inflate the case against the provider.
	LANDowntimeSeconds int64 `json:"lan_downtime_seconds"`

	// ISPAvailabilityPct is the number to cite: uptime as a fraction of
	// monitored time, counting only provider-attributable failures.
	ISPAvailabilityPct float64 `json:"isp_availability_pct"`
	// TotalAvailabilityPct additionally counts local faults.
	TotalAvailabilityPct float64 `json:"total_availability_pct"`

	OutageCount    int `json:"outage_count"`
	ISPOutageCount int `json:"isp_outage_count"`

	LongestOutageSeconds int64  `json:"longest_outage_seconds"`
	LongestOutageClass   string `json:"longest_outage_class"`

	// MTBFSeconds is mean time between ISP-attributable failures.
	MTBFSeconds int64 `json:"mtbf_seconds"`

	Interval int64 `json:"interval_seconds"`
}

// Outage is one recorded outage session.
type Outage struct {
	ID        int64     `json:"id"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
	Ongoing   bool      `json:"ongoing"`
	Duration  int64     `json:"duration_seconds"`
	Class     string    `json:"class"`
	BlamesISP bool      `json:"blames_isp"`
	Detail    string    `json:"detail"`
}

// Status is the current live state, for the dashboard's indicator.
type Status struct {
	LastCycle time.Time `json:"last_cycle"`
	Verdict   string    `json:"verdict"`
	Stale     bool      `json:"stale"`
	Gateway   string    `json:"gateway"`
	ISPEdge   string    `json:"isp_edge"`
	Interface string    `json:"interface"`
}

// Bucket is one slice of the timeline strip.
type Bucket struct {
	Start time.Time `json:"start"`
	// Counts is verdict -> number of cycles in this bucket.
	Counts map[string]int `json:"counts"`
	// Dominant is the worst verdict seen in the bucket, so a brief outage stays
	// visible instead of being averaged away by its neighbours.
	Dominant string `json:"dominant"`
	Cycles   int    `json:"cycles"`
}

// LinkChange records a period on a given interface/SSID. A change invalidates
// comparison across the window and is surfaced rather than hidden.
type LinkChange struct {
	Interface string    `json:"interface"`
	SSID      string    `json:"ssid"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	Cycles    int       `json:"cycles"`
}

// BuildSummary computes the headline statistics for [since, until].
func BuildSummary(st *store.Store, since, until time.Time, interval time.Duration) (*Summary, error) {
	db := st.DB()
	secs := int64(interval.Seconds())
	if secs <= 0 {
		secs = 5
	}

	s := &Summary{
		Since:          since,
		Until:          until,
		WindowSeconds:  int64(until.Sub(since).Seconds()),
		VerdictSeconds: map[string]int64{},
		Interval:       secs,
	}

	rows, err := db.Query(
		`SELECT verdict, COUNT(*) FROM cycles WHERE ts >= ? AND ts <= ? GROUP BY verdict`,
		since.Unix(), until.Unix())
	if err != nil {
		return nil, fmt.Errorf("query cycles: %w", err)
	}
	defer rows.Close()

	var observed int64
	for rows.Next() {
		var verdict string
		var count int64
		if err := rows.Scan(&verdict, &count); err != nil {
			return nil, err
		}
		observed += count
		sec := count * secs
		s.VerdictSeconds[verdict] = sec
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	s.MonitoredSeconds = observed * secs
	if s.WindowSeconds > 0 {
		s.CoveragePct = pct(s.MonitoredSeconds, s.WindowSeconds)
	}

	outages, spans, err := loadOutages(st, since, until, secs)
	if err != nil {
		return nil, err
	}
	s.OutageCount = len(outages)
	for i, o := range outages {
		if o.BlamesISP {
			s.ISPOutageCount++
			s.ISPDowntimeSeconds += spans[i].winDur
		} else if classify.Verdict(o.Class) == classify.LANFault {
			s.LANDowntimeSeconds += spans[i].winDur
		}
		if o.Duration > s.LongestOutageSeconds {
			s.LongestOutageSeconds = o.Duration
			s.LongestOutageClass = o.Class
		}
	}

	if s.MonitoredSeconds > 0 {
		ispUp := s.MonitoredSeconds - s.ISPDowntimeSeconds
		if ispUp < 0 {
			ispUp = 0
		}
		totalUp := s.MonitoredSeconds - s.ISPDowntimeSeconds - s.LANDowntimeSeconds
		if totalUp < 0 {
			totalUp = 0
		}
		s.ISPAvailabilityPct = pct(ispUp, s.MonitoredSeconds)
		s.TotalAvailabilityPct = pct(totalUp, s.MonitoredSeconds)
	}
	// Mean time between provider failures, over monitored time only.
	if s.ISPOutageCount > 0 {
		s.MTBFSeconds = s.MonitoredSeconds / int64(s.ISPOutageCount)
	}

	return s, nil
}

type outageSpan struct {
	clampedStart int64
	winDur       int64
	blamesISP    bool
}

// Outages lists outage sessions overlapping the window, newest first.
func Outages(st *store.Store, since, until time.Time) ([]Outage, error) {
	out, _, err := loadOutages(st, since, until, 5)
	return out, err
}

func loadOutages(st *store.Store, since, until time.Time, secs int64) ([]Outage, []outageSpan, error) {
	if secs <= 0 {
		secs = 5
	}
	rows, err := st.DB().Query(`
		SELECT id, started, COALESCE(ended, 0), COALESCE(duration_s, 0), class, COALESCE(detail, '')
		FROM outages
		WHERE started <= ? AND (ended IS NULL OR ended >= ?)
		ORDER BY started DESC`, until.Unix(), since.Unix())
	if err != nil {
		return nil, nil, fmt.Errorf("query outages: %w", err)
	}

	type rawOutage struct {
		id      int64
		started int64
		ended   int64
		dur     int64
		class   string
		detail  string
	}
	var raws []rawOutage
	for rows.Next() {
		var r rawOutage
		if err := rows.Scan(&r.id, &r.started, &r.ended, &r.dur, &r.class, &r.detail); err != nil {
			rows.Close()
			return nil, nil, err
		}
		raws = append(raws, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()

	var out []Outage
	var spans []outageSpan
	sinceUnix := since.Unix()
	untilUnix := until.Unix()

	for _, r := range raws {
		o := Outage{
			ID:        r.id,
			Started:   time.Unix(r.started, 0),
			Duration:  r.dur,
			Class:     r.class,
			BlamesISP: classify.Verdict(r.class).BlamesISP(),
			Detail:    r.detail,
		}

		var lastBad int64
		if r.ended == 0 {
			o.Ongoing = true
			lastBad, o.Duration = ongoingOutageBounds(st, r.started, untilUnix, secs)
		} else {
			o.Ended = time.Unix(r.ended, 0)
			lastBad = r.ended
			if o.Duration <= r.ended-r.started {
				diff := r.ended - r.started
				if diff < 0 {
					diff = 0
				}
				o.Duration = diff + secs
			}
		}

		clampedStart := r.started
		if sinceUnix > clampedStart {
			clampedStart = r.started + ((sinceUnix-r.started+secs-1)/secs)*secs
		}
		clampedEnd := lastBad
		if untilUnix < clampedEnd {
			if untilUnix < r.started {
				clampedEnd = r.started - secs
			} else {
				clampedEnd = r.started + ((untilUnix-r.started)/secs)*secs
			}
		}
		var winDur int64
		if clampedEnd >= clampedStart {
			if !o.Ongoing && r.started >= sinceUnix && r.ended <= untilUnix {
				winDur = o.Duration
			} else {
				winDur = (clampedEnd - clampedStart) + secs
			}
		}

		out = append(out, o)
		spans = append(spans, outageSpan{
			clampedStart: clampedStart,
			winDur:       winDur,
			blamesISP:    o.BlamesISP,
		})
	}
	return out, spans, nil
}

func ongoingOutageBounds(st *store.Store, started, untilUnix, secs int64) (lastBad, duration int64) {
	var maxBad int64
	_ = st.DB().QueryRow(`
		SELECT COALESCE(MAX(ts), 0) FROM cycles
		WHERE ts >= ? AND ts <= ? AND verdict IN ('LAN_FAULT', 'ISP_FAULT', 'ISP_UPSTREAM')`,
		started, untilUnix).Scan(&maxBad)
	if maxBad >= started {
		return maxBad, (maxBad - started) + secs
	}
	var totalCycles int64
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM cycles`).Scan(&totalCycles)
	if totalCycles > 0 {
		return started, secs
	}
	endUnix := untilUnix
	if now := time.Now().Unix(); now < endUnix && now >= started {
		endUnix = now
	}
	dur := endUnix - started
	if dur < secs {
		dur = secs
	}
	return started + dur - secs, dur
}

// CurrentStatus reports the most recent cycle and whether it is stale.
func CurrentStatus(st *store.Store, interval time.Duration) (*Status, error) {
	s := &Status{
		Gateway:   st.GetMeta("gateway"),
		ISPEdge:   st.GetMeta("isp_edge"),
		Interface: st.GetMeta("interface"),
	}
	var ts int64
	err := st.DB().QueryRow(`SELECT ts, verdict FROM cycles ORDER BY ts DESC LIMIT 1`).Scan(&ts, &s.Verdict)
	if err == sql.ErrNoRows {
		s.Stale = true
		s.Verdict = string(classify.Unknown)
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.LastCycle = time.Unix(ts, 0)
	// More than a few intervals without a cycle means the collector is not
	// running, which is a different situation from the internet being down.
	s.Stale = time.Since(s.LastCycle) > 4*interval
	return s, nil
}

// Timeline buckets cycles for the strip chart.
func Timeline(st *store.Store, since, until time.Time, buckets int) ([]Bucket, error) {
	if buckets < 1 {
		buckets = 120
	}
	span := until.Sub(since)
	if span <= 0 {
		return nil, nil
	}
	width := span / time.Duration(buckets)
	if width < time.Second {
		width = time.Second
		buckets = int(span / width)
	}

	rows, err := st.DB().Query(
		`SELECT ts, verdict FROM cycles WHERE ts >= ? AND ts <= ? ORDER BY ts`,
		since.Unix(), until.Unix())
	if err != nil {
		return nil, fmt.Errorf("query timeline: %w", err)
	}
	defer rows.Close()

	out := make([]Bucket, buckets)
	for i := range out {
		out[i] = Bucket{
			Start:  since.Add(time.Duration(i) * width),
			Counts: map[string]int{},
		}
	}

	for rows.Next() {
		var ts int64
		var verdict string
		if err := rows.Scan(&ts, &verdict); err != nil {
			return nil, err
		}
		idx := int(time.Unix(ts, 0).Sub(since) / width)
		if idx < 0 || idx >= buckets {
			continue
		}
		out[idx].Counts[verdict]++
		out[idx].Cycles++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		out[i].Dominant = dominant(out[i].Counts)
	}
	return out, nil
}

// dominant returns the most serious verdict present, not the most frequent.
// A 30-second outage inside a 10-minute bucket must not be smoothed into "OK".
func dominant(counts map[string]int) string {
	if len(counts) == 0 {
		return string(classify.Unknown)
	}
	order := []classify.Verdict{
		classify.ISPFault, classify.ISPUpstream, classify.LANFault,
		classify.Degraded, classify.ICMPDeprio, classify.OK,
	}
	for _, v := range order {
		if counts[string(v)] > 0 {
			return string(v)
		}
	}
	// Unrecognised verdict: fall back to whichever is most common.
	type kv struct {
		k string
		n int
	}
	var all []kv
	for k, n := range counts {
		all = append(all, kv{k, n})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
	return all[0].k
}

// HourHistogram returns ISP-attributable downtime seconds per hour of day,
// which is what surfaces peak-hour congestion patterns.
func HourHistogram(st *store.Store, since, until time.Time, interval time.Duration) ([24]int64, error) {
	var hist [24]int64
	secs := int64(interval.Seconds())
	if secs <= 0 {
		secs = 5
	}

	_, spans, err := loadOutages(st, since, until, secs)
	if err != nil {
		return hist, err
	}

	for _, sp := range spans {
		if !sp.blamesISP || sp.winDur <= 0 {
			continue
		}
		cur := time.Unix(sp.clampedStart, 0).In(time.Local)
		end := time.Unix(sp.clampedStart+sp.winDur, 0).In(time.Local)
		for cur.Before(end) {
			intoHour := time.Duration(cur.Minute())*time.Minute + time.Duration(cur.Second())*time.Second
			nextHour := cur.Add(time.Hour - intoHour)
			if nextHour.After(end) {
				nextHour = end
			}
			hist[cur.Hour()] += int64(nextHour.Sub(cur).Seconds())
			cur = nextHour
		}
	}
	return hist, nil
}

// Links lists the interface/SSID combinations seen in the window.
func Links(st *store.Store, since, until time.Time) ([]LinkChange, error) {
	rows, err := st.DB().Query(`
		SELECT COALESCE(iface, ''), COALESCE(ssid, ''), MIN(ts), MAX(ts), COUNT(*)
		FROM cycles WHERE ts >= ? AND ts <= ?
		GROUP BY iface, ssid ORDER BY MIN(ts)`, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LinkChange
	for rows.Next() {
		var lc LinkChange
		var first, last int64
		if err := rows.Scan(&lc.Interface, &lc.SSID, &first, &last, &lc.Cycles); err != nil {
			return nil, err
		}
		lc.First, lc.Last = time.Unix(first, 0), time.Unix(last, 0)
		out = append(out, lc)
	}
	return out, rows.Err()
}

func pct(num, den int64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den) * 100
}
