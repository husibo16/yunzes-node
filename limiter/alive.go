package limiter

import "sync"

// AliveStore is the concurrency-safe owner of the per-uid alive-IP counter
// that the panel reports via /v1/server/alivelist. It replaces the bare
// map[int]int that used to live on Limiter.AliveList — the data path
// (CheckLimit) reads it on every connection while the control plane
// (nodeInfoMonitor / UpdateUser) writes it from a different goroutine.
//
// The store is intentionally tiny: only the three operations the limiter
// actually needs (Get, Replace, Delete) are exported, so callers cannot
// reach the underlying map and reintroduce a race.
type AliveStore struct {
	mu sync.RWMutex
	m  map[int]int
}

// NewAliveStore wraps an initial uid->alive-ip map. A nil seed is treated
// as an empty store. The seed is copied so callers cannot mutate the
// internal map after handing it over.
func NewAliveStore(seed map[int]int) *AliveStore {
	s := &AliveStore{m: make(map[int]int, len(seed))}
	for k, v := range seed {
		s.m[k] = v
	}
	return s
}

// Get returns the alive-IP count for uid. Returns 0 if uid is unknown.
func (s *AliveStore) Get(uid int) int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	v := s.m[uid]
	s.mu.RUnlock()
	return v
}

// Replace swaps the entire alive map atomically. The passed map is copied
// so the caller can keep using it (and so the previous snapshot stays
// valid for any in-flight Get).
func (s *AliveStore) Replace(next map[int]int) {
	if s == nil {
		return
	}
	dup := make(map[int]int, len(next))
	for k, v := range next {
		dup[k] = v
	}
	s.mu.Lock()
	s.m = dup
	s.mu.Unlock()
}

// Delete removes a uid from the store. No-op if absent. Used by
// Limiter.UpdateUser when the panel reports a user removal.
func (s *AliveStore) Delete(uid int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.m, uid)
	s.mu.Unlock()
}

// Len returns the current number of tracked uids. Test-only helper.
func (s *AliveStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}
