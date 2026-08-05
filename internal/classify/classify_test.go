package classify

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want Verdict
	}{
		{
			name: "everything reachable",
			in:   Input{GatewayOK: true, ISPEdgeOK: true, InternetUp: 3, InternetTotal: 3},
			want: OK,
		},
		{
			name: "router down is the user's own fault, not the ISP's",
			in:   Input{GatewayOK: false, ISPEdgeOK: false, InternetUp: 0, InternetTotal: 3},
			want: LANFault,
		},
		{
			name: "router down outranks everything else being down",
			in:   Input{GatewayOK: false, ISPEdgeOK: true, InternetUp: 3, InternetTotal: 3},
			want: LANFault,
		},
		{
			name: "router up, ISP edge and internet gone",
			in:   Input{GatewayOK: true, ISPEdgeOK: false, InternetUp: 0, InternetTotal: 3},
			want: ISPFault,
		},
		{
			name: "ISP edge alive but nothing beyond it",
			in:   Input{GatewayOK: true, ISPEdgeOK: true, InternetUp: 0, InternetTotal: 3},
			want: ISPUpstream,
		},
		{
			name: "silent ISP edge while internet works is cosmetic",
			in:   Input{GatewayOK: true, ISPEdgeOK: false, InternetUp: 3, InternetTotal: 3},
			want: ICMPDeprio,
		},
		{
			name: "one anchor down is degraded, not an outage",
			in:   Input{GatewayOK: true, ISPEdgeOK: true, InternetUp: 2, InternetTotal: 3},
			want: Degraded,
		},
		{
			name: "no anchors configured is unknown, never an outage",
			in:   Input{GatewayOK: true, ISPEdgeOK: true, InternetUp: 0, InternetTotal: 0},
			want: Unknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in); got != tc.want {
				t.Errorf("Classify(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The attribution rules are what the entire conclusion rests on, so they get
// their own explicit test rather than being implied by the table above.
func TestAttribution(t *testing.T) {
	if !LANFault.IsOutage() {
		t.Error("LAN_FAULT should count as an outage")
	}
	if LANFault.BlamesISP() {
		t.Error("LAN_FAULT must never be attributed to the ISP")
	}
	for _, v := range []Verdict{ISPFault, ISPUpstream} {
		if !v.IsOutage() || !v.BlamesISP() {
			t.Errorf("%v should be an ISP-attributable outage", v)
		}
	}
	for _, v := range []Verdict{OK, Degraded, ICMPDeprio, Unknown} {
		if v.IsOutage() {
			t.Errorf("%v must not count as an outage", v)
		}
		if v.BlamesISP() {
			t.Errorf("%v must not be blamed on the ISP", v)
		}
	}
}

// feed runs a verdict sequence through a tracker at 5s intervals and collects
// the events it emits.
func feed(tr *Tracker, start time.Time, verdicts []Verdict) []Event {
	var events []Event
	for i, v := range verdicts {
		ts := start.Add(time.Duration(i) * 5 * time.Second)
		if ev := tr.Observe(ts, v); ev != nil {
			events = append(events, *ev)
		}
	}
	return events
}

func TestSingleDroppedCycleIsNotAnOutage(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tr := NewTracker(3, 3)

	events := feed(tr, base, []Verdict{OK, OK, ISPFault, OK, OK, OK})

	if len(events) != 0 {
		t.Fatalf("a single bad cycle produced %d event(s); debounce is not working: %+v", len(events), events)
	}
	if tr.InOutage() {
		t.Error("tracker should not be in an outage")
	}
}

func TestTwoBadCyclesBelowThresholdIsNotAnOutage(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tr := NewTracker(3, 3)

	events := feed(tr, base, []Verdict{OK, ISPFault, ISPFault, OK, OK, OK})

	if len(events) != 0 {
		t.Fatalf("2 bad cycles with threshold 3 should not open an outage, got %+v", events)
	}
}

func TestThreeBadCyclesOpenAnOutage(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tr := NewTracker(3, 3)

	events := feed(tr, base, []Verdict{OK, ISPFault, ISPFault, ISPFault})

	if len(events) != 1 || !events[0].Opened {
		t.Fatalf("expected one open event, got %+v", events)
	}
	// The outage began at index 1, not at index 3 where we noticed it.
	wantStart := base.Add(5 * time.Second)
	if !events[0].Start.Equal(wantStart) {
		t.Errorf("outage start = %v, want %v (must backdate to the first bad cycle)",
			events[0].Start, wantStart)
	}
	if events[0].Class != ISPFault {
		t.Errorf("class = %v, want %v", events[0].Class, ISPFault)
	}
}

func TestOutageClosesAtLastFailureNotAtRecovery(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tr := NewTracker(3, 3)

	// bad at indices 0,1,2 then good from index 3.
	events := feed(tr, base, []Verdict{
		ISPFault, ISPFault, ISPFault, OK, OK, OK,
	})

	if len(events) != 2 {
		t.Fatalf("expected open then close, got %+v", events)
	}
	closed := events[1]
	if !closed.Closed {
		t.Fatalf("second event should be a close: %+v", closed)
	}
	if !closed.Start.Equal(base) {
		t.Errorf("start = %v, want %v", closed.Start, base)
	}
	// The last failure was index 2; recovery was noticed at index 5.
	wantEnd := base.Add(10 * time.Second)
	if !closed.End.Equal(wantEnd) {
		t.Errorf("end = %v, want %v (must be the last failure, not the moment of recovery)",
			closed.End, wantEnd)
	}
}

func TestOutageStaysOpenThroughBriefRecovery(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tr := NewTracker(3, 3)

	// One good cycle in the middle must not close a flapping outage.
	events := feed(tr, base, []Verdict{
		ISPFault, ISPFault, ISPFault, OK, ISPFault, ISPFault,
	})

	if len(events) != 1 || !events[0].Opened {
		t.Fatalf("expected only an open event, got %+v", events)
	}
	if !tr.InOutage() {
		t.Error("outage should still be open after a single good cycle")
	}
}

func TestOutageEscalatesToISPFault(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tr := NewTracker(3, 3)

	// Starts looking like local trouble, then proves to be the ISP.
	events := feed(tr, base, []Verdict{
		LANFault, LANFault, LANFault, ISPFault, OK, OK, OK,
	})

	if len(events) != 2 {
		t.Fatalf("expected open then close, got %+v", events)
	}
	if events[1].Class != ISPFault {
		t.Errorf("class = %v, want %v: a session containing an ISP failure is the ISP's",
			events[1].Class, ISPFault)
	}
}

func TestPureLANOutageNeverBlamesISP(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tr := NewTracker(3, 3)

	events := feed(tr, base, []Verdict{
		LANFault, LANFault, LANFault, LANFault, OK, OK, OK,
	})

	if len(events) != 2 {
		t.Fatalf("expected open then close, got %+v", events)
	}
	if events[1].Class.BlamesISP() {
		t.Errorf("a wholly local outage was attributed to the ISP as %v", events[1].Class)
	}
}

func TestDegradedNeverOpensAnOutage(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tr := NewTracker(3, 3)

	events := feed(tr, base, []Verdict{
		Degraded, Degraded, Degraded, ICMPDeprio, ICMPDeprio, ICMPDeprio,
	})

	if len(events) != 0 {
		t.Fatalf("degraded/cosmetic verdicts must not open an outage, got %+v", events)
	}
}
