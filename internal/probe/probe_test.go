package probe

import (
	"context"
	"testing"
	"time"
)

// TestUnprivilegedSocket guards the assumption the whole design rests on: that
// macOS hands out ICMP datagram sockets to a non-root user.
func TestUnprivilegedSocket(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatalf("could not open unprivileged ICMP socket: %v", err)
	}
	defer p.Close()
}

// TestProbeLive needs a working network; skipped under -short.
func TestProbeLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live network probe")
	}
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	// 192.0.2.1 is TEST-NET-1 and is guaranteed unroutable, so it exercises the
	// timeout path alongside a target that should answer.
	res := p.Probe(context.Background(), []string{"1.1.1.1", "192.0.2.1"}, 2, 2*time.Second)

	if got := res["1.1.1.1"]; !got.OK() {
		t.Errorf("1.1.1.1 unreachable: sent=%d recv=%d", got.Sent, got.Recv)
	} else {
		t.Logf("1.1.1.1 rtt=%v loss=%.0f%%", got.RTT, got.LossPct())
	}

	if got := res["192.0.2.1"]; got.OK() {
		t.Errorf("TEST-NET-1 answered, which should be impossible: %+v", got)
	} else if got.LossPct() != 100 {
		t.Errorf("TEST-NET-1 loss = %.0f%%, want 100%%", got.LossPct())
	}
}

// TestProbeConcurrentTargets checks that probing many targets on one socket
// still costs roughly one timeout, not one per target.
func TestProbeConcurrentTargets(t *testing.T) {
	if testing.Short() {
		t.Skip("live network probe")
	}
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	start := time.Now()
	res := p.Probe(context.Background(),
		[]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, 1, 1*time.Second)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("three dead targets took %v; probes are not concurrent", elapsed)
	}
	for addr, r := range res {
		if r.OK() {
			t.Errorf("%s answered unexpectedly", addr)
		}
	}
}

// TestProbeDeadlineCoversAllSendRounds verifies that when (count-1)*100ms >= timeout,
// the reader deadline does not expire before the final echo round is sent and answered.
func TestProbeDeadlineCoversAllSendRounds(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	// 3 echoes are spaced by 2 * 100ms = 200ms. With timeout = 150ms, computing
	// deadline before the send loop expires the reader at 150ms before the 3rd
	// echo is sent at 200ms.
	res := p.Probe(context.Background(), []string{"127.0.0.1"}, 3, 150*time.Millisecond)
	got := res["127.0.0.1"]
	if got.Sent != 3 || got.Recv != 3 {
		t.Errorf("127.0.0.1 sent=%d recv=%d, want sent=3 recv=3", got.Sent, got.Recv)
	}
}
