package limiter

import (
	"sync"
	"time"
)

// oldUserOnlineTTL bounds how long an entry stays in OldUserOnline
// before ClearOnlineIP's periodic GC sweep evicts it.
//
// The entry's actual claim window is "between GetOnlineDevice draining
// UserOnlineIP and the next CheckLimit call that LoadOrStore-misses
// for the same (taguuid, ip)". That window is dominated by the
// userReportPeriodic interval (default 30 s) plus client-reconnect
// timing. Ten minutes is two orders of magnitude longer than any
// realistic claim window and easily evicts the long-tail IPs that
// never come back.
//
// Tuning down further would risk evicting an entry an active user is
// about to reclaim; tuning up further wastes memory linearly with the
// union of every IP that has ever connected.
const oldUserOnlineTTL = 10 * time.Minute

// oldUserOnlineEntry pairs the original (uid) value with the time the
// store accepted it. GCStale uses insertedAt to evict stale entries.
type oldUserOnlineEntry struct {
	uid        int
	insertedAt time.Time
}

// OldUserOnlineStore is the concurrency-safe owner of the per-ip
// "this user was just here" cache that GetOnlineDevice populates and
// CheckLimit consumes. Without an active GC the previous bare
// sync.Map grew monotonically — entries were only deleted when the
// SAME uid reconnected from the SAME ip, leaving every other lifecycle
// (user disconnects entirely, user reconnects from a different ip,
// ip gets reassigned to a different uid) leaking forever.
//
// Same shape as AliveStore (C6) and trafficStore (C7) — a small
// dedicated store rather than spreading sync.Map.Range / Delete
// timestamp logic across the data-path call site.
type OldUserOnlineStore struct {
	m sync.Map // key: string ip, value: *oldUserOnlineEntry
}

// NewOldUserOnlineStore constructs an empty store. AddLimiter calls it
// once per controller; tests can call it standalone.
func NewOldUserOnlineStore() *OldUserOnlineStore {
	return &OldUserOnlineStore{}
}

// Store records (ip -> uid) at time.Now(). Overwrites any prior
// entry for the same ip — the timestamp resets, which matches the
// "this user was JUST here" intent (the latest GetOnlineDevice
// observation should win, not the earliest).
func (s *OldUserOnlineStore) Store(ip string, uid int) {
	if s == nil {
		return
	}
	s.m.Store(ip, &oldUserOnlineEntry{uid: uid, insertedAt: time.Now()})
}

// Load returns the stored uid for ip if present, else (0, false).
// Does NOT consider TTL — callers can read entries between GC sweeps;
// the device-limit logic in CheckLimit treats both fresh and slightly-
// stale entries the same way.
func (s *OldUserOnlineStore) Load(ip string) (int, bool) {
	if s == nil {
		return 0, false
	}
	v, ok := s.m.Load(ip)
	if !ok {
		return 0, false
	}
	return v.(*oldUserOnlineEntry).uid, true
}

// Delete removes ip's entry. No-op if absent or on nil receiver.
// Used by CheckLimit when the same uid reconnects to the same ip.
func (s *OldUserOnlineStore) Delete(ip string) {
	if s == nil {
		return
	}
	s.m.Delete(ip)
}

// GCStale evicts every entry whose insertedAt is older than
// (time.Now() - maxAge). Returns the number of evictions for telemetry.
//
// sync.Map.Range explicitly supports concurrent Delete from inside the
// callback; GCStale can run from the ClearOnlineIP periodic without
// blocking the data-path Load/Store/Delete on the same map.
//
// A non-positive maxAge is treated as "no GC" — returns 0 without
// taking the map. This keeps the call-site invocation tolerant if a
// future caller passes the operator-configured TTL through verbatim.
func (s *OldUserOnlineStore) GCStale(maxAge time.Duration) int {
	if s == nil || maxAge <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	evicted := 0
	s.m.Range(func(k, v any) bool {
		if v.(*oldUserOnlineEntry).insertedAt.Before(cutoff) {
			s.m.Delete(k)
			evicted++
		}
		return true
	})
	return evicted
}

// Len returns the current number of tracked entries. Test-only helper.
func (s *OldUserOnlineStore) Len() int {
	if s == nil {
		return 0
	}
	n := 0
	s.m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
