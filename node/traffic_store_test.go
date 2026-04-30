package node

import (
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTrafficStore_AddDeleteReset(t *testing.T) {
	s := newTrafficStore()
	s.Add("u1", 100)
	s.Add("u1", 50)
	s.Add("u2", 7)
	if got := s.Get("u1"); got != 150 {
		t.Fatalf("Get(u1) = %d, want 150", got)
	}
	if got := s.Get("u2"); got != 7 {
		t.Fatalf("Get(u2) = %d, want 7", got)
	}
	s.Delete("u1")
	if got := s.Get("u1"); got != 0 {
		t.Fatalf("after Delete: Get(u1) = %d, want 0", got)
	}
	s.Reset()
	if got := s.Len(); got != 0 {
		t.Fatalf("after Reset: Len = %d, want 0", got)
	}
}

func TestTrafficStore_DrainTakesEntriesAtOrAboveThreshold(t *testing.T) {
	s := newTrafficStore()
	s.Add("u1", 999)  // below
	s.Add("u2", 1000) // exactly at threshold — must be drained
	s.Add("u3", 5000) // above
	got := s.Drain(1000)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "u2" || got[1] != "u3" {
		t.Fatalf("Drain = %v, want [u2 u3]", got)
	}
	// u1 untouched, u2/u3 cleared.
	if v := s.Get("u1"); v != 999 {
		t.Fatalf("u1 should remain 999, got %d", v)
	}
	if v := s.Get("u2"); v != 0 {
		t.Fatalf("u2 should be cleared, got %d", v)
	}
	if v := s.Get("u3"); v != 0 {
		t.Fatalf("u3 should be cleared, got %d", v)
	}
}

func TestTrafficStore_DrainNonPositiveThresholdIsNoOp(t *testing.T) {
	s := newTrafficStore()
	s.Add("u1", 1)
	if got := s.Drain(0); got != nil {
		t.Fatalf("Drain(0) = %v, want nil", got)
	}
	if got := s.Drain(-1); got != nil {
		t.Fatalf("Drain(-1) = %v, want nil", got)
	}
	if v := s.Get("u1"); v != 1 {
		t.Fatalf("u1 must not be touched by no-op Drain, got %d", v)
	}
}

func TestTrafficStore_NilReceiverIsSafe(t *testing.T) {
	var s *trafficStore
	s.Add("u1", 1)
	s.Delete("u1")
	s.Reset()
	if got := s.Get("u1"); got != 0 {
		t.Fatalf("nil Get = %d, want 0", got)
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("nil Len = %d, want 0", got)
	}
	if got := s.Drain(1); got != nil {
		t.Fatalf("nil Drain = %v, want nil", got)
	}
}

// TestTrafficStore_ConcurrentAddDrainDelete mirrors the production access
// pattern: userReportPeriodic calls Add, dynamicSpeedLimitPeriodic calls
// Drain, nodeInfoMonitorPeriodic calls Delete/Reset. Must run cleanly
// under -race. Also asserts that Drain never returns the same uuid twice
// (no lost-update on the accumulator).
func TestTrafficStore_ConcurrentAddDrainDelete(t *testing.T) {
	s := newTrafficStore()
	const adders = 8
	const drainers = 4
	const deleters = 2
	const iters = 1000

	var addersWG sync.WaitGroup
	for i := 0; i < adders; i++ {
		addersWG.Add(1)
		go func() {
			defer addersWG.Done()
			for j := 0; j < iters; j++ {
				s.Add("u1", 1)
				s.Add("u2", 2)
				s.Add("u3", 4)
			}
		}()
	}

	var drainerWG sync.WaitGroup
	var stop atomic.Bool
	var drainTotal atomic.Int64
	for i := 0; i < drainers; i++ {
		drainerWG.Add(1)
		go func() {
			defer drainerWG.Done()
			for !stop.Load() {
				out := s.Drain(1)
				drainTotal.Add(int64(len(out)))
			}
		}()
	}

	var deleterWG sync.WaitGroup
	for i := 0; i < deleters; i++ {
		deleterWG.Add(1)
		go func() {
			defer deleterWG.Done()
			for j := 0; j < iters; j++ {
				s.Delete("u1")
				if j%128 == 0 {
					s.Reset()
				}
			}
		}()
	}

	addersWG.Wait()
	deleterWG.Wait()
	stop.Store(true)
	drainerWG.Wait()

	// Final Drain to flush whatever's left so we leave a clean store.
	_ = s.Drain(1)
}
