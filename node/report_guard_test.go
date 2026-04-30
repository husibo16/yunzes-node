package node

import (
	"testing"
)

// TestReportRollbackDecision is the C12 traffic-guard policy lock-in.
//
// Default cap is 5. Sequence of failures 1..5 should all roll back
// (shouldDrop=false). Failure 6 is the first one that drops.
func TestReportRollbackDecision_DefaultCap(t *testing.T) {
	cases := []struct {
		failures int
		drop     bool
	}{
		{1, false},
		{2, false},
		{3, false},
		{4, false},
		{5, false}, // exactly at cap, still roll back
		{6, true},  // first to exceed cap, drop
		{7, true},
		{100, true},
	}
	for _, c := range cases {
		got := reportRollbackDecision(c.failures, 0) // 0 = default
		if got != c.drop {
			t.Errorf("default cap, failures=%d: drop=%v, want %v", c.failures, got, c.drop)
		}
	}
}

func TestReportRollbackDecision_ExplicitCap(t *testing.T) {
	// Operator-supplied cap of 2.
	if got := reportRollbackDecision(1, 2); got {
		t.Error("failures=1, cap=2: must NOT drop")
	}
	if got := reportRollbackDecision(2, 2); got {
		t.Error("failures=2, cap=2: must NOT drop (boundary)")
	}
	if got := reportRollbackDecision(3, 2); !got {
		t.Error("failures=3, cap=2: MUST drop")
	}
}

func TestReportRollbackDecision_DisabledByNegative(t *testing.T) {
	// Operator chose unbounded rollback (legacy behavior). Even after
	// 1000 consecutive failures, never drop.
	if got := reportRollbackDecision(1000, -1); got {
		t.Error("cap=-1: drop must never fire")
	}
}
