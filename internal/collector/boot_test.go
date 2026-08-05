package collector

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/config"
	"github.com/mohammadshamma/monitor-internet/internal/store"
)

// These cover the boot-time path: on a real reboot the collector starts before
// the network is configured, `route -n get default` reports no gateway, and an
// earlier version treated that as fatal — which under launchd's KeepAlive turns
// into a crash-loop against the restart throttle instead of a short wait.

// errNoRoute is what discovery actually returns before the network is up.
var errNoRoute = errors.New("discover default route: no default gateway in route output")

// bootCollector builds a Collector whose discovery step is scripted and whose
// backoff is short enough to test in milliseconds.
func bootCollector(t *testing.T, discover func(context.Context) error) *Collector {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	c, err := New(config.Default(), st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	c.discover = discover
	c.retryInitial = 2 * time.Millisecond
	c.retryMax = 20 * time.Millisecond
	return c
}

func TestDiscoveryRetriesUntilTheNetworkAppears(t *testing.T) {
	var calls int32
	c := bootCollector(t, func(context.Context) error {
		// Fail the first four attempts, as a slow boot would.
		if atomic.AddInt32(&calls, 1) <= 4 {
			return errNoRoute
		}
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- c.discoverWithRetry(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("discoverWithRetry = %v, want nil once the network appears", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("discoverWithRetry never returned; it is not retrying, it is stuck")
	}

	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Errorf("discovery called %d times, want 5 (4 failures then success)", got)
	}
}

func TestDiscoverySucceedsFirstTryWithoutWaiting(t *testing.T) {
	var calls int32
	c := bootCollector(t, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	start := time.Now()
	if err := c.discoverWithRetry(context.Background()); err != nil {
		t.Fatalf("discoverWithRetry = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v on the happy path; it should not back off at all", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("discovery called %d times, want 1", got)
	}
}

// A network that never comes up must not spin forever: cancelling has to break
// the loop promptly, including while it is sleeping between attempts.
func TestDiscoveryStopsPromptlyOnCancel(t *testing.T) {
	c := bootCollector(t, func(context.Context) error { return errNoRoute })
	c.retryInitial = 250 * time.Millisecond
	c.retryMax = 250 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.discoverWithRetry(ctx) }()

	time.Sleep(30 * time.Millisecond) // land inside a backoff sleep
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("want a cancellation error from discoverWithRetry")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not interrupt the backoff sleep")
	}
}

// The regression guard for the actual bug: Run must exit cleanly rather than
// returning an error, because cmd/mon passes a non-nil error to log.Fatalf.
// Before the fix this returned the discovery error and the process exited 1,
// which launchd's KeepAlive turned into a restart loop.
func TestRunExitsCleanlyWhenTheNetworkNeverAppears(t *testing.T) {
	c := bootCollector(t, func(context.Context) error { return errNoRoute })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil: a network that never appears is not a fatal error, "+
				"or the process crash-loops under launchd", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context expired")
	}
}

// While waiting for a route, nothing may be written: an unmonitored window has
// to reduce coverage, never be recorded as downtime we did not observe.
func TestNoCyclesRecordedBeforeTheNetworkIsUp(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	c, err := New(config.Default(), st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	defer c.Close()
	c.discover = func(context.Context) error { return errNoRoute }
	c.retryInitial = 2 * time.Millisecond
	c.retryMax = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)

	var cycles int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM cycles`).Scan(&cycles); err != nil {
		t.Fatalf("count cycles: %v", err)
	}
	if cycles != 0 {
		t.Errorf("recorded %d cycles while waiting for a route; that window must stay "+
			"a coverage gap, not invented data", cycles)
	}
}
