// Package classify turns raw per-tier probe results into an attributable
// verdict, and turns a stream of verdicts into outage sessions.
//
// This is the correctness core of the tool. A wrong answer here does not crash
// anything — it quietly produces a confident, wrong conclusion about who is at
// fault, which is the worst possible failure mode for evidence meant to settle
// an argument with a provider.
package classify

import "time"

// Verdict is the classification of a single probe cycle.
type Verdict string

const (
	// OK means every tier answered.
	OK Verdict = "OK"
	// LANFault means the local router did not answer. Whatever else is broken,
	// the evidence cannot reach past the user's own equipment, so this is never
	// counted against the ISP.
	LANFault Verdict = "LAN_FAULT"
	// ISPFault means the router answered but neither the ISP's edge nor the
	// wider internet did. This is the strongest evidence against a provider.
	ISPFault Verdict = "ISP_FAULT"
	// ISPUpstream means the ISP's own edge answered but nothing beyond it did —
	// their transit or peering, still their responsibility.
	ISPUpstream Verdict = "ISP_UPSTREAM"
	// ICMPDeprio means the ISP edge ignored us while real traffic flowed fine.
	// Routers commonly rate-limit ICMP aimed at their own interface; treating
	// this as an outage would manufacture failures that never happened.
	ICMPDeprio Verdict = "ICMP_DEPRIO"
	// Degraded means some internet anchors were reachable and some were not.
	Degraded Verdict = "DEGRADED"
	// Unknown marks an interval where the collector was not running. Excluded
	// from both uptime and downtime; it reduces coverage instead.
	Unknown Verdict = "UNKNOWN"
)

// Input is one cycle's per-tier outcome.
type Input struct {
	GatewayOK bool
	// ISPEdgeOK is false when the first ISP hop did not answer. Note this alone
	// never triggers an outage.
	ISPEdgeOK bool
	// InternetUp / InternetTotal count reachable public anchors.
	InternetUp    int
	InternetTotal int
}

// Classify maps one cycle's probe results to a verdict.
//
// The ordering is deliberate: the gateway is checked first, so a local failure
// can never be misattributed to the provider.
func Classify(in Input) Verdict {
	if !in.GatewayOK {
		return LANFault
	}

	switch {
	case in.InternetTotal == 0:
		return Unknown
	case in.InternetUp == 0:
		// Nothing on the public internet is reachable. Whether the ISP's own
		// edge answers only changes which part of their network to blame.
		if !in.ISPEdgeOK {
			return ISPFault
		}
		return ISPUpstream
	case in.InternetUp < in.InternetTotal:
		return Degraded
	default:
		// Everything beyond the ISP works, so a silent edge hop is cosmetic.
		if !in.ISPEdgeOK {
			return ICMPDeprio
		}
		return OK
	}
}

// IsOutage reports whether a verdict represents loss of connectivity.
func (v Verdict) IsOutage() bool {
	switch v {
	case LANFault, ISPFault, ISPUpstream:
		return true
	}
	return false
}

// BlamesISP reports whether a verdict counts as provider-attributable downtime.
// LANFault is an outage but explicitly not the provider's fault.
func (v Verdict) BlamesISP() bool {
	switch v {
	case ISPFault, ISPUpstream:
		return true
	}
	return false
}

// Event is emitted by Tracker when an outage session opens or closes.
type Event struct {
	Opened bool
	Closed bool
	Start  time.Time
	End    time.Time
	Class  Verdict
	Cycles int
}

// Tracker debounces a stream of verdicts into outage sessions.
//
// An outage opens only after FailThreshold consecutive bad cycles and closes
// after ClearThreshold consecutive good ones, so one dropped packet cannot
// manufacture an outage. When it does open, the start time is backdated to the
// first bad cycle rather than the cycle that crossed the threshold — otherwise
// every recorded outage would be systematically short by the threshold.
type Tracker struct {
	FailThreshold  int
	ClearThreshold int

	inOutage bool

	// pending accumulates consecutive bad cycles not yet promoted to an outage.
	pendingStart time.Time
	pendingCount int
	pendingClass Verdict

	// goodRun counts consecutive good cycles while an outage is open.
	goodRun int
	// lastBad is the timestamp of the most recent bad cycle, which becomes the
	// outage's end time — not the moment we noticed recovery.
	lastBad   time.Time
	openStart time.Time
	openClass Verdict
	openCount int
}

// NewTracker builds a Tracker with the given thresholds.
func NewTracker(fail, clear int) *Tracker {
	if fail < 1 {
		fail = 1
	}
	if clear < 1 {
		clear = 1
	}
	return &Tracker{FailThreshold: fail, ClearThreshold: clear}
}

// InOutage reports whether an outage session is currently open.
func (t *Tracker) InOutage() bool { return t.inOutage }

// Observe feeds one cycle in and returns any resulting event.
func (t *Tracker) Observe(ts time.Time, v Verdict) *Event {
	if v.IsOutage() {
		return t.observeBad(ts, v)
	}
	return t.observeGood(ts)
}

func (t *Tracker) observeBad(ts time.Time, v Verdict) *Event {
	t.lastBad = ts
	t.goodRun = 0

	if t.inOutage {
		t.openCount++
		// Escalate: an outage that starts as LAN trouble but proves to be the
		// ISP should be recorded as the more specific ISP fault.
		t.openClass = worse(t.openClass, v)
		return nil
	}

	if t.pendingCount == 0 {
		t.pendingStart = ts
		t.pendingClass = v
	} else {
		t.pendingClass = worse(t.pendingClass, v)
	}
	t.pendingCount++

	if t.pendingCount >= t.FailThreshold {
		t.inOutage = true
		t.openStart = t.pendingStart
		t.openClass = t.pendingClass
		t.openCount = t.pendingCount
		ev := &Event{
			Opened: true,
			Start:  t.openStart, // backdated to the first bad cycle
			Class:  t.openClass,
			Cycles: t.openCount,
		}
		t.pendingCount = 0
		return ev
	}
	return nil
}

func (t *Tracker) observeGood(ts time.Time) *Event {
	// A single good cycle breaks a pending run that never became an outage.
	t.pendingCount = 0

	if !t.inOutage {
		return nil
	}

	t.goodRun++
	if t.goodRun < t.ClearThreshold {
		return nil
	}

	ev := &Event{
		Closed: true,
		Start:  t.openStart,
		End:    t.lastBad, // recovery happened at the last failure, not now
		Class:  t.openClass,
		Cycles: t.openCount,
	}
	t.inOutage = false
	t.goodRun = 0
	t.openCount = 0
	return ev
}

// worse picks the more specific / more damning of two outage classes, so a
// session containing any ISP failure is recorded as an ISP failure.
func worse(a, b Verdict) Verdict {
	rank := func(v Verdict) int {
		switch v {
		case ISPFault:
			return 3
		case ISPUpstream:
			return 2
		case LANFault:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}
