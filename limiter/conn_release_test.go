package limiter

import (
	"sync"
	"testing"
)

// rangeCount counts entries in a sync.Map.
func rangeCount(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestDelConnCount_StoresIPNotUserKey is the regression for Bug A: the
// previous DelConnCount wrote `is.Store(user, ...)` instead of
// `is.Store(ip, ...)`. The wrong-key write injected a phantom user-keyed
// entry that AddConnCount's `ips.Range(cn++)` would later count as an
// online IP, inflating the user's IP count.
func TestDelConnCount_StoresIPNotUserKey(t *testing.T) {
	cl := NewConnLimiter(0, 100, true)
	user, ip := "u1", "1.2.3.4"

	// Two TCP from same IP -> internal count = 4.
	cl.AddConnCount(user, ip, true)
	cl.AddConnCount(user, ip, true)

	cl.DelConnCount(user, ip)

	is, ok := cl.ip.Load(user)
	if !ok {
		t.Fatalf("c.ip[%q] missing after DelConnCount", user)
	}
	im := is.(*sync.Map)

	// Inner map must contain ONLY `ip`, never `user`.
	if _, leaked := im.Load(user); leaked {
		t.Fatalf("phantom user-keyed entry leaked into inner ip map; this is the Bug A regression")
	}
	v, ok := im.Load(ip)
	if !ok {
		t.Fatalf("expected inner map to still hold %q after one of two DelConnCount calls", ip)
	}
	if v.(int) != 2 {
		t.Fatalf("inner count for %q = %d, want 2 (4 -> 2 after one DelConnCount)", ip, v.(int))
	}
	if got := rangeCount(im); got != 1 {
		t.Fatalf("inner map size = %d, want 1 (only the legitimate ip entry)", got)
	}
}

// TestDelConnCount_DropsUserWhenInnerMapEmpties is the regression for
// Bug B: the previous DelConnCount ranged over c.ip (outer map) instead
// of `is` (inner per-user map), so the user's empty inner map was never
// reaped from c.ip.
func TestDelConnCount_DropsUserWhenInnerMapEmpties(t *testing.T) {
	cl := NewConnLimiter(0, 10, true)

	// Two users so the outer map is non-empty even after we close u1.
	// The previous bug ranged over c.ip; with two users it would have
	// always seen non-empty and never deleted u1.
	cl.AddConnCount("u1", "ip1", true)
	cl.AddConnCount("u2", "ip2", true)

	cl.DelConnCount("u1", "ip1")

	if _, present := cl.ip.Load("u1"); present {
		t.Fatalf("u1 entry should be reaped from c.ip after its inner map emptied; this is the Bug B regression")
	}
	if _, present := cl.ip.Load("u2"); !present {
		t.Fatalf("u2 entry incorrectly removed; only u1 should be reaped")
	}
}

// TestDelConnCount_ReleasesIPLimitSlotForNextConnect is the end-to-end
// "realtime mode could only go up" regression: a user at the IP limit
// must be able to connect from a fresh IP after closing one of the
// existing ones.
func TestDelConnCount_ReleasesIPLimitSlotForNextConnect(t *testing.T) {
	cl := NewConnLimiter(0, 2, true)

	// Fill u1 to the IP limit (2 IPs).
	if cl.AddConnCount("u1", "ip-a", true) {
		t.Fatalf("first connect must succeed")
	}
	if cl.AddConnCount("u1", "ip-b", true) {
		t.Fatalf("second connect must succeed")
	}
	// Third IP must be rejected at the limit.
	if !cl.AddConnCount("u1", "ip-c", true) {
		t.Fatalf("third connect must be limited at IP cap")
	}

	// Close one of the active IPs.
	cl.DelConnCount("u1", "ip-a")

	// Third IP can now connect.
	if cl.AddConnCount("u1", "ip-c", true) {
		t.Fatalf("after release, third connect must succeed; this is the realtime-only-goes-up regression")
	}
}

// TestDelConnCount_ConnLimitDecrementsAcrossModes verifies that the
// connLimit counter (plain int, populated in both realtime and non-
// realtime AddConnCount paths) gets decremented in BOTH modes.
// Previously DelConnCount short-circuited on !realtime, so non-realtime
// mode could only ever increase its conn count.
func TestDelConnCount_ConnLimitDecrementsAcrossModes(t *testing.T) {
	for _, realtime := range []bool{true, false} {
		realtime := realtime
		t.Run(label(realtime), func(t *testing.T) {
			cl := NewConnLimiter(2, 0, realtime)

			if cl.AddConnCount("u1", "ip", true) {
				t.Fatalf("first connect must succeed")
			}
			if cl.AddConnCount("u1", "ip", true) {
				t.Fatalf("second connect must succeed")
			}
			// At cap, third must be rejected.
			if !cl.AddConnCount("u1", "ip", true) {
				t.Fatalf("third connect must be limited at conn cap")
			}

			cl.DelConnCount("u1", "ip")
			if cl.AddConnCount("u1", "ip", true) {
				t.Fatalf("after release, next connect must succeed (realtime=%v)", realtime)
			}
		})
	}
}

func label(realtime bool) string {
	if realtime {
		return "realtime"
	}
	return "non-realtime"
}

// TestDelConnCount_OneTcpAtCount2DeletesEntry covers the boundary where
// AddConnCount-TCP stored exactly 2 (single TCP, no UDP) and a single
// DelConnCount drains it.
func TestDelConnCount_OneTcpAtCount2DeletesEntry(t *testing.T) {
	cl := NewConnLimiter(0, 10, true)
	cl.AddConnCount("u1", "ip1", true)
	cl.DelConnCount("u1", "ip1")
	if _, ok := cl.ip.Load("u1"); ok {
		t.Fatalf("u1 entry should be reaped after the only IP closed")
	}
}

// TestDelConnCount_UdpPlusTcpRetainsUdpAfterTcpClose covers the mixed
// case: UDP first stored 1, TCP then incremented to 3. Closing the TCP
// (count -> 1) must leave the entry behind so the UDP slot survives.
func TestDelConnCount_UdpPlusTcpRetainsUdpAfterTcpClose(t *testing.T) {
	cl := NewConnLimiter(0, 10, true)
	cl.AddConnCount("u1", "ip1", false) // UDP -> stores 1
	cl.AddConnCount("u1", "ip1", true)  // TCP on already-online IP -> 1+2=3
	cl.DelConnCount("u1", "ip1")        // TCP closes -> 3-2=1
	is, ok := cl.ip.Load("u1")
	if !ok {
		t.Fatalf("u1 entry should remain (UDP still active)")
	}
	v, ok := is.(*sync.Map).Load("ip1")
	if !ok {
		t.Fatalf("ip1 entry should remain after TCP close (UDP still active)")
	}
	if v.(int) != 1 {
		t.Fatalf("ip1 count = %d, want 1 (UDP only)", v.(int))
	}
}

// TestDelConnCount_NonRealtimeIPBranchSkipped verifies that DelConnCount
// does not mutate the IP-counter map in non-realtime mode (where it
// stores time.Time, not int). The connLimit counter is unaffected by
// this gate and is exercised by the cross-mode test above.
func TestDelConnCount_NonRealtimeIPBranchSkipped(t *testing.T) {
	cl := NewConnLimiter(0, 10, false)
	cl.AddConnCount("u1", "ip1", true) // stores time.Time
	cl.DelConnCount("u1", "ip1")
	is, ok := cl.ip.Load("u1")
	if !ok {
		t.Fatalf("u1 entry should still be present")
	}
	if got := rangeCount(is.(*sync.Map)); got != 1 {
		t.Fatalf("inner map size = %d, want 1 (untouched)", got)
	}
}

// TestDelConnCount_ConcurrentAddDelIsRaceClean exercises the access
// pattern from the data path: many goroutines AddConnCount + DelConnCount
// in pairs. Must be -race clean.
func TestDelConnCount_ConcurrentAddDelIsRaceClean(t *testing.T) {
	cl := NewConnLimiter(0, 1000, true)
	const workers = 32
	const iters = 500

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			user := "u"
			ip := ipFor(id)
			for j := 0; j < iters; j++ {
				cl.AddConnCount(user, ip, true)
				cl.DelConnCount(user, ip)
			}
		}(i)
	}
	wg.Wait()
}

func ipFor(id int) string {
	return "ip-" + itoa(id)
}

// tiny inline strconv.Itoa to keep test deps minimal
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// TestIsRealtime is a sanity check for the accessor used by the data-path
// hook installation logic.
func TestIsRealtime(t *testing.T) {
	if !NewConnLimiter(0, 10, true).IsRealtime() {
		t.Fatal("IsRealtime should be true")
	}
	if NewConnLimiter(0, 10, false).IsRealtime() {
		t.Fatal("IsRealtime should be false")
	}
}
