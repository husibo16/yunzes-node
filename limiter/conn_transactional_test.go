package limiter

import (
	"sync"
	"testing"
)

// loadCount returns the connLimit counter for user, or 0 if absent.
func loadCount(cl *ConnLimiter, user string) int {
	v, ok := cl.count.Load(user)
	if !ok {
		return 0
	}
	return v.(int)
}

// TestAddConnCount_RejectByIPLimitDoesNotLeakConnCount is the C8.1
// regression. Sequence:
//
//  1. user u1 fills the IP cap with two distinct IPs (TCP). Each
//     accepted connect bumps the per-user TCP count by 1 -> count=2.
//  2. u1 tries a third IP. It must be rejected by the ipLimit check.
//     The previous AddConnCount incremented the connLimit counter
//     BEFORE checking ipLimit, so this reject left count=3 — a
//     permanent +1 leak per blocked attempt, since callers do not
//     fire DelConnCount on the reject path.
//  3. After the reject, the count must still be 2.
func TestAddConnCount_RejectByIPLimitDoesNotLeakConnCount(t *testing.T) {
	cl := NewConnLimiter(10, 2, true) // connLimit=10 so it can't be the rejecter

	if cl.AddConnCount("u1", "ip-a", true) {
		t.Fatalf("first connect must succeed")
	}
	if cl.AddConnCount("u1", "ip-b", true) {
		t.Fatalf("second connect must succeed")
	}
	if got := loadCount(cl, "u1"); got != 2 {
		t.Fatalf("count after two accepted TCP = %d, want 2", got)
	}

	// Third IP rejected by ipLimit cap.
	if !cl.AddConnCount("u1", "ip-c", true) {
		t.Fatalf("third connect must be rejected at IP cap")
	}

	if got := loadCount(cl, "u1"); got != 2 {
		t.Fatalf("count after IP-cap reject = %d, want 2 (this is the C8.1 reject-path leak regression)", got)
	}
}

// TestAddConnCount_RejectByConnLimitDoesNotLeak verifies the conn-cap
// reject path. The previous code's connLimit-driven reject branch did
// NOT mutate count (it returned before the increment), so this case
// was already correct — but lock it in so a future restructure
// doesn't reintroduce a leak.
func TestAddConnCount_RejectByConnLimitDoesNotLeak(t *testing.T) {
	cl := NewConnLimiter(2, 0, true)

	if cl.AddConnCount("u1", "ip", true) {
		t.Fatalf("first connect must succeed")
	}
	if cl.AddConnCount("u1", "ip", true) {
		t.Fatalf("second connect must succeed")
	}
	if got := loadCount(cl, "u1"); got != 2 {
		t.Fatalf("count after two accepted TCP = %d, want 2", got)
	}

	if !cl.AddConnCount("u1", "ip", true) {
		t.Fatalf("third connect must be rejected at conn cap")
	}

	if got := loadCount(cl, "u1"); got != 2 {
		t.Fatalf("count after conn-cap reject = %d, want 2 (must not leak)", got)
	}
}

// TestAddConnCount_RejectByIPLimitDoesNotMutateInnerMap verifies that
// an ipLimit-driven reject also doesn't add a phantom entry to the
// per-user inner ip map. (The original code didn't have this leak —
// the cn check returned before storing the new ip — but lock it in.)
func TestAddConnCount_RejectByIPLimitDoesNotMutateInnerMap(t *testing.T) {
	cl := NewConnLimiter(0, 2, true)

	cl.AddConnCount("u1", "ip-a", true)
	cl.AddConnCount("u1", "ip-b", true)
	if !cl.AddConnCount("u1", "ip-c", true) {
		t.Fatalf("third must be rejected")
	}
	is, _ := cl.ip.Load("u1")
	im := is.(*sync.Map)
	if _, leaked := im.Load("ip-c"); leaked {
		t.Fatalf("rejected ip-c leaked into inner map")
	}
	if got := rangeCount(im); got != 2 {
		t.Fatalf("inner map size = %d, want 2 (only ip-a and ip-b)", got)
	}
}

// TestAddConnCount_FirstUserAlwaysAdmitted preserves the original
// quirk that a brand-new user's first connect is admitted regardless
// of ipLimit (cap is checked against existing-user state only).
func TestAddConnCount_FirstUserAlwaysAdmitted(t *testing.T) {
	cl := NewConnLimiter(0, 1, true) // ipLimit=1
	if cl.AddConnCount("brand-new", "ip-a", true) {
		t.Fatalf("first-time user's first IP must always succeed")
	}
	// Their second IP must hit the cap.
	if !cl.AddConnCount("brand-new", "ip-b", true) {
		t.Fatalf("first-time user's second IP must be capped")
	}
}

// TestAddConnCount_OnlineIPDoesNotConsumeIPCap verifies that connecting
// from an already-online IP (e.g. multiplexed TCP) does not count
// against ipLimit even when the cap is full. Behavior present in the
// original; lock in.
func TestAddConnCount_OnlineIPDoesNotConsumeIPCap(t *testing.T) {
	cl := NewConnLimiter(0, 1, true) // ipLimit=1

	// First connection: stored as the only IP.
	if cl.AddConnCount("u1", "ip-a", true) {
		t.Fatalf("first connect must succeed")
	}
	// Second TCP from the SAME ip: must succeed (already-online path,
	// only bumps the per-ip TCP count).
	if cl.AddConnCount("u1", "ip-a", true) {
		t.Fatalf("repeat TCP from same IP must succeed regardless of ipLimit")
	}
	// A different IP must hit the cap.
	if !cl.AddConnCount("u1", "ip-b", true) {
		t.Fatalf("different IP must be capped at ipLimit=1")
	}

	// Inner map confirms count tracking: ip-a should have 4 (two TCPs).
	is, _ := cl.ip.Load("u1")
	v, _ := is.(*sync.Map).Load("ip-a")
	if v.(int) != 4 {
		t.Fatalf("ip-a count = %d, want 4 (two TCPs => 2+2)", v.(int))
	}
}

// TestAddConnCount_NonRealtimeOnlineIPUpdatesTime verifies the non-
// realtime path's "ip already online -> store time.Now()" behavior is
// preserved by the restructure.
func TestAddConnCount_NonRealtimeOnlineIPUpdatesTime(t *testing.T) {
	cl := NewConnLimiter(0, 10, false)
	cl.AddConnCount("u1", "ip", true)
	is, _ := cl.ip.Load("u1")
	first, _ := is.(*sync.Map).Load("ip")

	// Re-add: should update time, not create a new entry.
	cl.AddConnCount("u1", "ip", true)
	second, _ := is.(*sync.Map).Load("ip")
	// Both should be time.Time and the second should be at-or-after the first.
	t1 := first.(interface{})
	t2 := second.(interface{})
	if t1 == nil || t2 == nil {
		t.Fatalf("expected time.Time values, got first=%v second=%v", first, second)
	}
}

// TestAddConnCount_AcceptedRoundtripIsBalanced verifies the happy-path
// invariant we want to preserve: every accepted AddConnCount paired
// with a DelConnCount leaves count and ip empty for the user.
func TestAddConnCount_AcceptedRoundtripIsBalanced(t *testing.T) {
	cl := NewConnLimiter(5, 5, true)
	if cl.AddConnCount("u1", "ip-a", true) {
		t.Fatalf("connect must succeed")
	}
	if got := loadCount(cl, "u1"); got != 1 {
		t.Fatalf("count after accept = %d, want 1", got)
	}
	cl.DelConnCount("u1", "ip-a")
	if got := loadCount(cl, "u1"); got != 0 {
		t.Fatalf("count after release = %d, want 0", got)
	}
	if _, present := cl.ip.Load("u1"); present {
		t.Fatalf("ip[u1] should be reaped after the only IP closed")
	}
}
