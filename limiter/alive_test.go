package limiter

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestAliveStore_BasicOps(t *testing.T) {
	s := NewAliveStore(map[int]int{1: 5, 2: 7})
	if got := s.Get(1); got != 5 {
		t.Fatalf("Get(1) = %d, want 5", got)
	}
	if got := s.Get(99); got != 0 {
		t.Fatalf("Get(99) = %d, want 0 (zero value for unknown uid)", got)
	}
	s.Delete(1)
	if got := s.Get(1); got != 0 {
		t.Fatalf("after Delete: Get(1) = %d, want 0", got)
	}
	s.Replace(map[int]int{42: 11})
	if got := s.Get(42); got != 11 {
		t.Fatalf("after Replace: Get(42) = %d, want 11", got)
	}
	if got := s.Get(2); got != 0 {
		t.Fatalf("after Replace: Get(2) = %d, want 0 (Replace must drop old keys)", got)
	}
}

func TestAliveStore_SeedIsCopied(t *testing.T) {
	seed := map[int]int{1: 1}
	s := NewAliveStore(seed)
	seed[1] = 99
	seed[2] = 99
	if got := s.Get(1); got != 1 {
		t.Fatalf("seed mutation leaked: Get(1) = %d, want 1", got)
	}
	if got := s.Get(2); got != 0 {
		t.Fatalf("seed mutation leaked: Get(2) = %d, want 0", got)
	}
}

func TestAliveStore_ReplaceIsCopied(t *testing.T) {
	s := NewAliveStore(nil)
	next := map[int]int{1: 1}
	s.Replace(next)
	next[1] = 99
	next[2] = 99
	if got := s.Get(1); got != 1 {
		t.Fatalf("Replace input mutation leaked: Get(1) = %d, want 1", got)
	}
	if got := s.Get(2); got != 0 {
		t.Fatalf("Replace input mutation leaked: Get(2) = %d, want 0", got)
	}
}

func TestAliveStore_NilReceiverIsSafe(t *testing.T) {
	var s *AliveStore
	if got := s.Get(1); got != 0 {
		t.Fatalf("nil Get = %d, want 0", got)
	}
	s.Delete(1)
	s.Replace(map[int]int{1: 1})
	if got := s.Len(); got != 0 {
		t.Fatalf("nil Len = %d, want 0", got)
	}
}

// TestAliveStore_ConcurrentReadWrite mirrors the production access pattern:
// the data path (CheckLimit) calls Get on every connection while the control
// plane (nodeInfoMonitor / UpdateUser) calls Replace and Delete from a
// different goroutine. Must run cleanly under `go test -race`.
func TestAliveStore_ConcurrentReadWrite(t *testing.T) {
	s := NewAliveStore(map[int]int{1: 1})

	const readers = 16
	const replacers = 4
	const deleters = 4
	const iters = 2000

	var writers sync.WaitGroup
	var stop atomic.Bool

	for i := 0; i < replacers; i++ {
		writers.Add(1)
		go func(seed int) {
			defer writers.Done()
			for j := 0; j < iters; j++ {
				s.Replace(map[int]int{1: seed + j, 2: seed - j, 3: j})
			}
		}(i * 1000)
	}
	for i := 0; i < deleters; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < iters; j++ {
				s.Delete(1)
				s.Delete(2)
				s.Delete(3)
			}
		}()
	}

	var readersWG sync.WaitGroup
	for i := 0; i < readers; i++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for !stop.Load() {
				_ = s.Get(1)
				_ = s.Get(2)
				_ = s.Get(3)
				_ = s.Get(999)
			}
		}()
	}

	writers.Wait()
	stop.Store(true)
	readersWG.Wait()
}
