package node

import "sync"

// trafficStore is the concurrency-safe accumulator that feeds the dynamic
// speed limit. Three goroutines reach for it:
//
//   - userReportPeriodic (reportUserTrafficTask): adds bytes after a
//     successful report so the panel-side report and the local dynamic
//     limit count the same chunk exactly once.
//   - dynamicSpeedLimitPeriodic (SpeedChecker): drains uuids that crossed
//     the configured byte threshold so UpdateDynamicSpeedLimit is fired
//     once per crossing.
//   - nodeInfoMonitorPeriodic: resets on full-reload and deletes per-user
//     entries when the panel reports user removals.
//
// The previous implementation was a bare map[string]int64 with zero
// synchronization, so activating the feeder would have turned a dormant
// feature into a fatal map-concurrent-write panic. Mirrors the small-
// surface shape of limiter.AliveStore.
type trafficStore struct {
	mu sync.Mutex
	m  map[string]int64
}

func newTrafficStore() *trafficStore {
	return &trafficStore{m: make(map[string]int64)}
}

// Add accumulates n bytes against uuid. No-op on nil receiver so the
// dynamic-limit feature stays opt-in (controllers without
// EnableDynamicSpeedLimit never allocate the store).
func (s *trafficStore) Add(uuid string, n int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.m[uuid] += n
	s.mu.Unlock()
}

// Delete removes a uuid (e.g. when the panel removes a user). No-op on
// nil receiver or unknown uuid.
func (s *trafficStore) Delete(uuid string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.m, uuid)
	s.mu.Unlock()
}

// Reset clears all accumulated bytes (used on full node-reload).
func (s *trafficStore) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.m = make(map[string]int64)
	s.mu.Unlock()
}

// Drain atomically removes every uuid whose accumulated byte count is at
// or above threshold and returns the uuids the caller should now flip to
// dynamic-limit mode. Removal-and-return happens under one lock, so a
// concurrent Add cannot lose bytes for an uuid that just tripped (the
// added bytes will accumulate against a fresh entry next cycle).
//
// A non-positive threshold is treated as "feature disabled" — returns
// nil without taking the lock. This keeps SpeedChecker cheap when an
// operator misconfigures Traffic to 0.
func (s *trafficStore) Drain(threshold int64) []string {
	if s == nil || threshold <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for uuid, t := range s.m {
		if t >= threshold {
			out = append(out, uuid)
			delete(s.m, uuid)
		}
	}
	return out
}

// Get returns the current byte count for uuid. Test-only helper.
func (s *trafficStore) Get(uuid string) int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	v := s.m[uuid]
	s.mu.Unlock()
	return v
}

// Len returns the current number of tracked uuids. Test-only helper.
func (s *trafficStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	n := len(s.m)
	s.mu.Unlock()
	return n
}
