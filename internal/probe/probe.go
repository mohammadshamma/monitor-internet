// Package probe sends ICMP echo requests using an unprivileged datagram
// socket.
//
// macOS grants SOCK_DGRAM/IPPROTO_ICMP sockets to ordinary users — /sbin/ping
// is not setuid yet works as an ordinary user — so this runs with no root
// and no subprocess. That matters for accuracy as much as tidiness: there is no
// per-probe process spawn adding jitter to the RTT, and no human-readable ping
// output to scrape.
package probe

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// Result is the outcome of probing one target for one cycle.
type Result struct {
	Addr string
	// Sent and Recv count echo requests and matching replies.
	Sent, Recv int
	// RTT is the best (minimum) round trip of the replies received. Minimum
	// rather than mean because it is the least contaminated by scheduling noise
	// and transient queueing — it is the closest thing to the true path latency.
	RTT time.Duration
}

// OK reports whether the target answered at least once.
func (r Result) OK() bool { return r.Recv > 0 }

// LossPct is the fraction of echoes that went unanswered, 0..100.
func (r Result) LossPct() float64 {
	if r.Sent == 0 {
		return 0
	}
	return float64(r.Sent-r.Recv) / float64(r.Sent) * 100
}

// Prober owns a single ICMP socket shared by all targets.
type Prober struct {
	conn *icmp.PacketConn
	mu   sync.Mutex
	seq  int
}

// New opens the unprivileged ICMP socket.
func New() (*Prober, error) {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("open unprivileged icmp socket: %w", err)
	}
	return &Prober{conn: conn}, nil
}

// Close releases the socket.
func (p *Prober) Close() error { return p.conn.Close() }

// nextSeq returns a fresh 16-bit sequence number.
func (p *Prober) nextSeq() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq = (p.seq + 1) & 0xffff
	return p.seq
}

// Probe sends count echoes to every address and waits up to timeout for
// replies. All targets share one socket and are probed concurrently, so a cycle
// costs one timeout regardless of how many targets there are.
//
// Replies are matched on sequence number, not on ICMP ID: for a "udp4" ICMP
// socket the kernel rewrites the echo ID to the socket's own port, so the ID we
// wrote is not the ID that comes back.
func (p *Prober) Probe(ctx context.Context, addrs []string, count int, timeout time.Duration) map[string]Result {
	results := make(map[string]Result, len(addrs))
	for _, a := range addrs {
		results[a] = Result{Addr: a}
	}

	type pending struct {
		addr string
		sent time.Time
	}
	inflight := make(map[int]pending)
	var mu sync.Mutex

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	// Reader runs for the whole window, collecting whatever arrives.
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			if err := p.conn.SetReadDeadline(deadline); err != nil {
				return
			}
			n, peer, err := p.conn.ReadFrom(buf)
			if err != nil {
				return // deadline reached or socket closed
			}
			now := time.Now()

			msg, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), buf[:n])
			if err != nil || msg.Type != ipv4.ICMPTypeEchoReply {
				continue // unreachable/time-exceeded are not successes
			}
			echo, ok := msg.Body.(*icmp.Echo)
			if !ok {
				continue
			}

			mu.Lock()
			pend, known := inflight[echo.Seq]
			if known && peerIP(peer) == pend.addr {
				delete(inflight, echo.Seq)
				r := results[pend.addr]
				r.Recv++
				if rtt := now.Sub(pend.sent); r.RTT == 0 || rtt < r.RTT {
					r.RTT = rtt
				}
				results[pend.addr] = r
			}
			mu.Unlock()
		}
	}()

	// Send every echo. Interleaved by round so a slow target does not delay the
	// others' first probe.
	for i := 0; i < count; i++ {
		for _, addr := range addrs {
			ip := net.ParseIP(addr)
			if ip == nil {
				continue
			}
			seq := p.nextSeq()
			msg := icmp.Message{
				Type: ipv4.ICMPTypeEcho,
				Code: 0,
				Body: &icmp.Echo{
					ID:   os.Getpid() & 0xffff,
					Seq:  seq,
					Data: []byte("monitor-internet"),
				},
			}
			wire, err := msg.Marshal(nil)
			if err != nil {
				continue
			}

			mu.Lock()
			inflight[seq] = pending{addr: addr, sent: time.Now()}
			r := results[addr]
			r.Sent++
			results[addr] = r
			mu.Unlock()

			if _, err := p.conn.WriteTo(wire, &net.UDPAddr{IP: ip}); err != nil {
				mu.Lock()
				delete(inflight, seq)
				mu.Unlock()
			}
		}
		if i < count-1 {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}

	<-done

	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]Result, len(results))
	for k, v := range results {
		out[k] = v
	}
	return out
}

func peerIP(a net.Addr) string {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.IP.String()
	case *net.IPAddr:
		return v.IP.String()
	default:
		return a.String()
	}
}
