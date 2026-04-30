package limiter

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/format"
	"github.com/husibo16/yunzes-node/conf"
)

// TestClearOnlineIP_GCsOldUserOnlineStaleEntries integrates the store
// with the periodic sweep — this is the path that actually runs in
// production. Build a real Limiter via AddLimiter, plant one stale +
// one fresh entry, run ClearOnlineIP, and verify only the stale
// entry was reaped.
func TestClearOnlineIP_GCsOldUserOnlineStaleEntries(t *testing.T) {
	const coreType = "xray"
	const tag = "test-gc-tag"

	// Init the package-level limiter map; safe to call multiple times
	// across the test binary thanks to map re-allocation.
	Init()
	defer DeleteLimiter(coreType, tag)

	users := []panel.UserInfo{{Id: 1, Uuid: "u1"}}
	lim := AddLimiter(coreType, tag, &conf.LimitConfig{}, users, nil)
	_ = format.RuntimeKey(coreType, tag) // sanity-touch the format helper

	lim.OldUserOnline.Store("stale", 99)
	if v, ok := lim.OldUserOnline.m.Load("stale"); ok {
		v.(*oldUserOnlineEntry).insertedAt = time.Now().Add(-2 * oldUserOnlineTTL)
	}
	lim.OldUserOnline.Store("fresh", 7)

	if err := ClearOnlineIP(); err != nil {
		t.Fatalf("ClearOnlineIP error: %v", err)
	}

	if _, ok := lim.OldUserOnline.Load("stale"); ok {
		t.Errorf("'stale' entry survived periodic GC sweep")
	}
	if _, ok := lim.OldUserOnline.Load("fresh"); !ok {
		t.Errorf("'fresh' entry was incorrectly evicted")
	}
}

func TestOldUserOnlineStore_BasicOps(t *testing.T) {
	s := NewOldUserOnlineStore()
	s.Store("1.2.3.4", 42)
	s.Store("5.6.7.8", 99)

	if uid, ok := s.Load("1.2.3.4"); !ok || uid != 42 {
		t.Fatalf("Load(1.2.3.4) = (%d, %v), want (42, true)", uid, ok)
	}
	if uid, ok := s.Load("5.6.7.8"); !ok || uid != 99 {
		t.Fatalf("Load(5.6.7.8) = (%d, %v), want (99, true)", uid, ok)
	}
	if _, ok := s.Load("missing"); ok {
		t.Fatalf("Load(missing) returned true")
	}

	s.Delete("1.2.3.4")
	if _, ok := s.Load("1.2.3.4"); ok {
		t.Fatalf("Load after Delete returned true")
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 (only 5.6.7.8 should remain)", got)
	}
}

// TestOldUserOnlineStore_StoreOverwritesAndResetsAge is the
// "latest-observation-wins" lock-in. GetOnlineDevice may Store the
// same ip on consecutive cycles; the timestamp must reset so a long-
// active user's entry does not get evicted as stale.
func TestOldUserOnlineStore_StoreOverwritesAndResetsAge(t *testing.T) {
	s := NewOldUserOnlineStore()
	s.Store("ip", 1)
	first, _ := s.m.Load("ip")
	firstTime := first.(*oldUserOnlineEntry).insertedAt
	time.Sleep(2 * time.Millisecond)
	s.Store("ip", 1)
	second, _ := s.m.Load("ip")
	secondTime := second.(*oldUserOnlineEntry).insertedAt
	if !secondTime.After(firstTime) {
		t.Fatalf("re-Store didn't refresh insertedAt: first=%v second=%v", firstTime, secondTime)
	}
}

// TestOldUserOnlineStore_GCStaleEvictsExpiredEntries is the C16
// regression. Pre-fix the store grew monotonically; with this GC
// path entries past TTL are dropped on the periodic sweep.
func TestOldUserOnlineStore_GCStaleEvictsExpiredEntries(t *testing.T) {
	s := NewOldUserOnlineStore()
	s.Store("stale", 1)
	// Backdate the entry by reaching into the internal struct — this is
	// a test-only assumption; if the on-disk shape changes the test
	// also changes, which is the right tradeoff vs. sleeping seconds.
	v, _ := s.m.Load("stale")
	v.(*oldUserOnlineEntry).insertedAt = time.Now().Add(-time.Hour)

	s.Store("fresh", 2) // current time

	evicted := s.GCStale(10 * time.Minute)
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1 (only 'stale' should be old)", evicted)
	}
	if _, ok := s.Load("stale"); ok {
		t.Errorf("'stale' was not evicted")
	}
	if _, ok := s.Load("fresh"); !ok {
		t.Errorf("'fresh' was incorrectly evicted")
	}
}

// TestOldUserOnlineStore_GCStaleBoundary covers the under-TTL vs
// past-TTL split with wide enough gaps that race-detector overhead
// can't push timestamps across the boundary. The exact-equals case
// is intentionally NOT tested — GCStale uses time.Now() internally,
// not a caller-provided anchor, so the equals-cutoff assertion would
// be flaky under race / slow CI.
func TestOldUserOnlineStore_GCStaleBoundary(t *testing.T) {
	s := NewOldUserOnlineStore()
	s.Store("under", 1)
	s.Store("over", 2)

	// Wide gaps relative to the 10-minute TTL: 1 minute under, 30
	// minutes over. Even with seconds of timing drift these stay on
	// the correct side of the cutoff.
	now := time.Now()
	if v, ok := s.m.Load("under"); ok {
		v.(*oldUserOnlineEntry).insertedAt = now.Add(-1 * time.Minute)
	}
	if v, ok := s.m.Load("over"); ok {
		v.(*oldUserOnlineEntry).insertedAt = now.Add(-30 * time.Minute)
	}

	evicted := s.GCStale(10 * time.Minute)
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1 ('over' is past TTL, 'under' is well within)", evicted)
	}
	if _, ok := s.Load("over"); ok {
		t.Errorf("'over' should have been evicted")
	}
	if _, ok := s.Load("under"); !ok {
		t.Errorf("'under' incorrectly evicted")
	}
}

// TestOldUserOnlineStore_GCStaleNonPositiveIsNoOp documents the
// caller-tolerance contract: a 0 / negative maxAge passes through
// without taking the map.
func TestOldUserOnlineStore_GCStaleNonPositiveIsNoOp(t *testing.T) {
	s := NewOldUserOnlineStore()
	s.Store("a", 1)
	if got := s.GCStale(0); got != 0 {
		t.Errorf("GCStale(0) = %d, want 0", got)
	}
	if got := s.GCStale(-time.Hour); got != 0 {
		t.Errorf("GCStale(-1h) = %d, want 0", got)
	}
	if _, ok := s.Load("a"); !ok {
		t.Errorf("non-positive maxAge must not evict")
	}
}

// TestOldUserOnlineStore_NilReceiverIsSafe — sing-only or partially-
// initialized Limiters may have a nil OldUserOnline pointer briefly
// during startup. None of the methods should panic.
func TestOldUserOnlineStore_NilReceiverIsSafe(t *testing.T) {
	var s *OldUserOnlineStore
	s.Store("x", 1)
	if uid, ok := s.Load("x"); ok || uid != 0 {
		t.Errorf("nil Load = (%d, %v), want (0, false)", uid, ok)
	}
	s.Delete("x")
	if got := s.GCStale(time.Hour); got != 0 {
		t.Errorf("nil GCStale = %d, want 0", got)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("nil Len = %d, want 0", got)
	}
}

// TestOldUserOnlineStore_ConcurrentStoreLoadDeleteGC mirrors the
// production access pattern: GetOnlineDevice (write-heavy) +
// CheckLimit (mixed Load/Delete) + ClearOnlineIP periodic GC. Must
// run cleanly under -race.
func TestOldUserOnlineStore_ConcurrentStoreLoadDeleteGC(t *testing.T) {
	s := NewOldUserOnlineStore()

	const writers = 8
	const readers = 8
	const iters = 1000

	// Writers do a fixed amount of work, then exit. Readers and the
	// GC sweeper run until stop fires, then exit. This shape mirrors
	// the equivalent stress test pattern from AliveStore (C6) and
	// trafficStore (C7).
	var writersWG sync.WaitGroup
	for i := 0; i < writers; i++ {
		writersWG.Add(1)
		go func(id int) {
			defer writersWG.Done()
			for j := 0; j < iters; j++ {
				ip := "ip-" + strconv.Itoa((id*100)+(j%50))
				s.Store(ip, id*1000+j)
			}
		}(i)
	}

	var readersWG sync.WaitGroup
	var stop atomic.Bool
	for i := 0; i < readers; i++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for !stop.Load() {
				_, _ = s.Load("ip-1")
				_, _ = s.Load("ip-100")
				s.Delete("ip-2")
			}
		}()
	}

	readersWG.Add(1)
	go func() {
		defer readersWG.Done()
		for !stop.Load() {
			// Aggressive cutoff to stress concurrent Range+Delete.
			s.GCStale(10 * time.Microsecond)
		}
	}()

	writersWG.Wait()
	stop.Store(true)
	readersWG.Wait()
}
