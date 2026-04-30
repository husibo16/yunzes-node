package node

import (
	"testing"

	"github.com/husibo16/yunzes-node/api/panel"
)

func TestSplitUserTrafficByThreshold(t *testing.T) {
	cases := []struct {
		name      string
		threshold int64
		in        []panel.UserTraffic
		wantAbove []int // UIDs expected in `above`
		wantBelow []int // UIDs expected in `belowOrEqual`
	}{
		{
			name:      "empty input",
			threshold: 100,
			in:        nil,
			wantAbove: nil,
			wantBelow: nil,
		},
		{
			name:      "all above threshold",
			threshold: 100,
			in: []panel.UserTraffic{
				{UID: 1, Upload: 200, Download: 0},
				{UID: 2, Upload: 50, Download: 60},  // 110 > 100
				{UID: 3, Upload: 0, Download: 101},
			},
			wantAbove: []int{1, 2, 3},
			wantBelow: nil,
		},
		{
			name:      "all at or below threshold",
			threshold: 100,
			in: []panel.UserTraffic{
				{UID: 1, Upload: 0, Download: 0},
				{UID: 2, Upload: 50, Download: 50}, // exactly 100, dropped per <=
				{UID: 3, Upload: 99, Download: 0},
			},
			wantAbove: nil,
			wantBelow: []int{1, 2, 3},
		},
		{
			name:      "mixed",
			threshold: 1024,
			in: []panel.UserTraffic{
				{UID: 1, Upload: 500, Download: 500},   // 1000 <= 1024 → below
				{UID: 2, Upload: 600, Download: 600},   // 1200 > 1024 → above
				{UID: 3, Upload: 0, Download: 1024},    // 1024 <= 1024 → below
				{UID: 4, Upload: 1, Download: 1024},    // 1025 > 1024 → above
				{UID: 5, Upload: 1_000_000, Download: 0}, // way above → above
			},
			wantAbove: []int{2, 4, 5},
			wantBelow: []int{1, 3},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			above, below := splitUserTrafficByThreshold(c.in, c.threshold)
			// Compare via normalised slices so nil and empty are treated
			// as equivalent — the function returns nil (`var above []T`)
			// for an empty side, and the test fixtures use nil for that
			// case, but reflect.DeepEqual would still trip on the
			// `make([]int, 0)` we'd otherwise build below.
			if !sameUIDOrder(above, c.wantAbove) {
				t.Errorf("above UIDs = %v, want %v", uidsOf(above), c.wantAbove)
			}
			if !sameUIDOrder(below, c.wantBelow) {
				t.Errorf("belowOrEqual UIDs = %v, want %v", uidsOf(below), c.wantBelow)
			}
		})
	}
}

// sameUIDOrder reports whether traffic carries exactly the UIDs in
// wantUIDs (order-sensitive). Treats nil and empty want-slices as
// equivalent — splitUserTrafficByThreshold legitimately returns nil
// for an empty side.
func sameUIDOrder(traffic []panel.UserTraffic, wantUIDs []int) bool {
	if len(traffic) != len(wantUIDs) {
		return false
	}
	for i, t := range traffic {
		if t.UID != wantUIDs[i] {
			return false
		}
	}
	return true
}

func uidsOf(traffic []panel.UserTraffic) []int {
	out := make([]int, len(traffic))
	for i, t := range traffic {
		out[i] = t.UID
	}
	return out
}

func TestSplitUserTrafficByThreshold_DoesNotAliasInput(t *testing.T) {
	// Strict no-loss requires that the caller can keep iterating the
	// pre-split slice for the online-user threshold check. Verify the
	// returned slices use distinct backing arrays so subsequent
	// in-place mutations on either don't corrupt the input.
	in := []panel.UserTraffic{
		{UID: 1, Upload: 100, Download: 0},
		{UID: 2, Upload: 5000, Download: 5000},
	}
	above, below := splitUserTrafficByThreshold(in, 1000)
	if len(above) > 0 && len(in) > 0 {
		// Mutating above must not change in.
		above[0].UID = 999
		if in[1].UID != 2 {
			t.Errorf("mutating above corrupted input: in=%+v", in)
		}
	}
	if len(below) > 0 && len(in) > 0 {
		below[0].UID = 888
		if in[0].UID != 1 {
			t.Errorf("mutating below corrupted input: in=%+v", in)
		}
	}
}

func TestReportThreshold_NilGuards(t *testing.T) {
	// Each successive layer is the kind of nil that can appear during
	// the first reportUserTrafficTask tick that fires before
	// nodeInfoMonitor has populated c.info / its Common / its Basic.
	// All three must return 0 instead of panicking.
	t.Run("nil controller info", func(t *testing.T) {
		c := &Controller{}
		if got := c.reportThreshold(); got != 0 {
			t.Errorf("nil c.info: got %d, want 0", got)
		}
	})
	t.Run("nil Common", func(t *testing.T) {
		c := &Controller{info: &panel.NodeInfo{}}
		if got := c.reportThreshold(); got != 0 {
			t.Errorf("nil Common: got %d, want 0", got)
		}
	})
	t.Run("nil Basic", func(t *testing.T) {
		c := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{}}}
		if got := c.reportThreshold(); got != 0 {
			t.Errorf("nil Basic: got %d, want 0", got)
		}
	})
	t.Run("threshold returned", func(t *testing.T) {
		c := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{Basic: &panel.BasicConfig{TrafficReportThreshold: 4096}}}}
		if got := c.reportThreshold(); got != 4096 {
			t.Errorf("got %d, want 4096", got)
		}
	})
}
