package main

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent runs of the macOS tailscale CLI race on the system VPN
// profile (2026-11-06: three simultaneous runs replaced the profile and
// left Tailscale stopped for twenty minutes), so every invocation must go
// through one lock.
func TestTailscaleOutputRunsOneCLIAtATime(t *testing.T) {
	old := tailscaleExec
	defer func() { tailscaleExec = old }()
	var inflight, peak int32
	tailscaleExec = func(ctx context.Context, args ...string) ([]byte, error) {
		n := atomic.AddInt32(&inflight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		return []byte("{}"), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tailscaleOutput(context.Background(), "status", "--json"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("peak concurrent tailscale CLI runs = %d, want 1", peak)
	}
}

// NEMESIS, 2026-11-05: the logon task started acp-mobile 19 s after boot,
// before Tailscale answered, and the process stayed localhost-only for four
// days.  Startup must keep asking until Tailscale is up.
func TestWaitForTailscaleRetriesUntilItAnswers(t *testing.T) {
	calls := 0
	self := func() (tailscaleInfo, error) {
		calls++
		if calls < 3 {
			return tailscaleInfo{}, errors.New("not up yet")
		}
		return tailscaleInfo{Hostname: "h.ts.net", IP: "100.1.2.3"}, nil
	}
	ts, err := waitForTailscale(self, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ts.IP != "100.1.2.3" || calls != 3 {
		t.Fatalf("ip = %q after %d calls, want 100.1.2.3 after 3", ts.IP, calls)
	}
}

func TestWaitForTailscaleGivesUpAtTheLimit(t *testing.T) {
	calls := 0
	self := func() (tailscaleInfo, error) {
		calls++
		return tailscaleInfo{}, errors.New("down")
	}
	start := time.Now()
	_, err := waitForTailscale(self, 5*time.Millisecond, 40*time.Millisecond)
	if err == nil {
		t.Fatal("err = nil, want the last failure")
	}
	if calls < 2 {
		t.Fatalf("calls = %d, want several attempts", calls)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waited %v, want about the 40ms limit", d)
	}
}

// A box without the tailscale CLI (test mode, CI) must start at once.
func TestWaitForTailscaleDoesNotWaitWhenTheCLIIsMissing(t *testing.T) {
	calls := 0
	self := func() (tailscaleInfo, error) {
		calls++
		return tailscaleInfo{}, &exec.Error{Name: "tailscale", Err: exec.ErrNotFound}
	}
	if _, err := waitForTailscale(self, time.Hour, 2*time.Hour); err == nil {
		t.Fatal("err = nil, want not-found")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}
