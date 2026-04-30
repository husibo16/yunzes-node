package node

import (
	"sync"
	"testing"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/format"
	"github.com/husibo16/yunzes-node/conf"
	"github.com/husibo16/yunzes-node/limiter"
)

var limiterInitOnce sync.Once

// limiter.AddLimiter writes to a package-level map that cmd/server.go's
// limiter.Init() allocates. Tests that exercise AddLimiter must allocate
// it themselves; once is enough per test binary.
func ensureLimiterInit() {
	limiterInitOnce.Do(limiter.Init)
}

func TestFeedDynamicTraffic_ResolvesUidToUuidAndAccumulates(t *testing.T) {
	c := &Controller{
		traffic: newTrafficStore(),
		userList: []panel.UserInfo{
			{Id: 1, Uuid: "uuid-1"},
			{Id: 2, Uuid: "uuid-2"},
			{Id: 3, Uuid: "uuid-3"},
		},
		Options: &conf.Options{
			LimitConfig: conf.LimitConfig{EnableDynamicSpeedLimit: true},
		},
	}

	c.feedDynamicTraffic([]panel.UserTraffic{
		{UID: 1, Upload: 100, Download: 50},
		{UID: 2, Upload: 0, Download: 7},
		{UID: 1, Upload: 25, Download: 25},    // second chunk for u1 in same cycle
		{UID: 999, Upload: 1234, Download: 0}, // unknown UID — must be dropped
	})

	if got := c.traffic.Get("uuid-1"); got != 200 {
		t.Fatalf("uuid-1 = %d, want 200 (100+50+25+25)", got)
	}
	if got := c.traffic.Get("uuid-2"); got != 7 {
		t.Fatalf("uuid-2 = %d, want 7", got)
	}
	if got := c.traffic.Get("uuid-3"); got != 0 {
		t.Fatalf("uuid-3 should be 0 (no traffic), got %d", got)
	}
	if got := c.traffic.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2 (uuid-1 + uuid-2; unknown UID dropped)", got)
	}
}

func TestFeedDynamicTraffic_NilTrafficStoreIsSafe(t *testing.T) {
	c := &Controller{
		traffic:  nil, // EnableDynamicSpeedLimit was false at startup
		userList: []panel.UserInfo{{Id: 1, Uuid: "uuid-1"}},
		Options:  &conf.Options{LimitConfig: conf.LimitConfig{}},
	}
	c.feedDynamicTraffic([]panel.UserTraffic{{UID: 1, Upload: 1, Download: 1}})
}

func TestSpeedChecker_TripsLimiterForUuidsOverThreshold(t *testing.T) {
	ensureLimiterInit()
	const coreType = "xray"
	const logicalTag = "test-tag"
	rk := format.RuntimeKey(coreType, logicalTag)

	users := []panel.UserInfo{
		{Id: 1, Uuid: "uuid-1"},
		{Id: 2, Uuid: "uuid-2"},
		{Id: 3, Uuid: "uuid-3"},
	}
	lim := limiter.AddLimiter(coreType, logicalTag, &conf.LimitConfig{}, users, nil)
	defer limiter.DeleteLimiter(coreType, logicalTag)

	c := &Controller{
		coreType:   coreType,
		logicalTag: logicalTag,
		runtimeKey: rk,
		limiter:    lim,
		traffic:    newTrafficStore(),
		Options: &conf.Options{
			LimitConfig: conf.LimitConfig{
				EnableDynamicSpeedLimit: true,
				DynamicSpeedLimitConfig: &conf.DynamicSpeedLimitConfig{
					Periodic:   60,
					Traffic:    1000,
					SpeedLimit: 5,
					ExpireTime: 1,
				},
			},
		},
	}

	c.traffic.Add("uuid-1", 999)  // below threshold
	c.traffic.Add("uuid-2", 1000) // exactly at threshold — must trip
	c.traffic.Add("uuid-3", 9999) // above

	if err := c.SpeedChecker(); err != nil {
		t.Fatalf("SpeedChecker error: %v", err)
	}

	// uuid-2 / uuid-3 should now have DynamicSpeedLimit set on the limiter.
	checkTripped := func(uuid string, want int) {
		t.Helper()
		v, ok := lim.UserLimitInfo.Load(format.UserTag(rk, uuid))
		if !ok {
			t.Fatalf("UserLimitInfo missing entry for %s", uuid)
		}
		info := v.(*limiter.UserLimitInfo)
		if info.DynamicSpeedLimit != want {
			t.Fatalf("%s DynamicSpeedLimit = %d, want %d", uuid, info.DynamicSpeedLimit, want)
		}
	}
	checkTripped("uuid-2", 5)
	checkTripped("uuid-3", 5)

	// uuid-1 must NOT have been tripped.
	v, _ := lim.UserLimitInfo.Load(format.UserTag(rk, "uuid-1"))
	if info, _ := v.(*limiter.UserLimitInfo); info != nil && info.DynamicSpeedLimit != 0 {
		t.Fatalf("uuid-1 unexpectedly tripped, DynamicSpeedLimit = %d", info.DynamicSpeedLimit)
	}

	// And c.traffic must have lost uuid-2 and uuid-3 (drained), kept uuid-1.
	if got := c.traffic.Get("uuid-1"); got != 999 {
		t.Fatalf("uuid-1 traffic = %d, want 999 (untouched)", got)
	}
	if got := c.traffic.Get("uuid-2"); got != 0 {
		t.Fatalf("uuid-2 traffic = %d, want 0 (drained)", got)
	}
	if got := c.traffic.Get("uuid-3"); got != 0 {
		t.Fatalf("uuid-3 traffic = %d, want 0 (drained)", got)
	}
}

func TestSpeedChecker_NilDynamicConfigIsNoop(t *testing.T) {
	c := &Controller{
		Options: &conf.Options{
			LimitConfig: conf.LimitConfig{
				EnableDynamicSpeedLimit: true,
				DynamicSpeedLimitConfig: nil,
			},
		},
	}
	if err := c.SpeedChecker(); err != nil {
		t.Fatalf("nil-config SpeedChecker error: %v", err)
	}
}
