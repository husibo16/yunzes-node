package node

import (
	"fmt"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/serverstatus"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) reportUserTrafficTask() (err error) {
	userTraffic, _ := c.server.GetUserTrafficSlice(c.runtimeKey, true)

	// Strict no-loss split: when the panel sets TrafficReportThreshold,
	// partition the drained slice into entries we will push (above
	// threshold) and entries we will roll back into core for
	// accumulation (at or below threshold). GetUserTrafficSlice with
	// reset=true already cleared the per-user counters; if we just
	// dropped sub-threshold bytes here they would be permanently lost.
	// Instead they ride along on the next AddUserTrafficSlice call so
	// that low-traffic users still eventually cross the threshold and
	// reach the panel — net counter parity with today's panel-side
	// filter, just delayed.
	var skipped []panel.UserTraffic
	if threshold := c.reportThreshold(); threshold > 0 {
		userTraffic, skipped = splitUserTrafficByThreshold(userTraffic, threshold)
	}

	if len(userTraffic) > 0 {
		err = c.apiClient.ReportUserTraffic(&userTraffic)
		if err != nil {
			c.reportFailures++
			if reportRollbackDecision(c.reportFailures, c.LimitConfig.MaxReportFailureRollbacks) {
				// Bounded-rollback guard: too many consecutive failures.
				// Drop EVERYTHING (filtered + skipped) on the floor so the
				// in-core per-user counter doesn't grow without limit.
				// Matches today's "drop bytes" semantics on outage.
				log.WithFields(mergeFields(c.logFields(), log.Fields{
					"err":                  err,
					"consecutive_failures": c.reportFailures,
					"dropped_count":        len(userTraffic) + len(skipped),
					"rollback_cap":         reportFailureCap(c.LimitConfig.MaxReportFailureRollbacks),
				})).Error("Report failed; rollback cap exceeded — traffic dropped to bound in-core accumulator")
			} else {
				// Roll BOTH wire-failed and sub-threshold bytes back to
				// core. Without rolling skipped together, an outage
				// would silently lose every sub-threshold delta drained
				// during failed cycles.
				rollback := userTraffic
				if len(skipped) > 0 {
					rollback = append(rollback, skipped...)
				}
				if rbErr := c.server.AddUserTrafficSlice(c.runtimeKey, rollback); rbErr != nil {
					log.WithFields(mergeFields(c.logFields(), log.Fields{
						"report_err":           err,
						"rollback_err":         rbErr,
						"consecutive_failures": c.reportFailures,
					})).Error("Report failed AND rollback failed — traffic lost")
				} else {
					log.WithFields(mergeFields(c.logFields(), log.Fields{
						"err":                  err,
						"rolled_back_count":    len(rollback),
						"consecutive_failures": c.reportFailures,
					})).Warn("Report failed, traffic rolled back to core for next cycle")
				}
			}
		} else {
			if c.reportFailures > 0 {
				log.WithFields(mergeFields(c.logFields(), log.Fields{
					"recovered_after_failures": c.reportFailures,
				})).Info("Report recovered after consecutive failures")
			}
			c.reportFailures = 0
			log.WithFields(c.logFields()).Infof("Report %d users traffic", len(userTraffic))
			if c.LimitConfig.EnableDynamicSpeedLimit {
				c.feedDynamicTraffic(userTraffic)
			}
			// Healthy report path: roll skipped back to core so the
			// strict no-loss invariant holds. The drained sub-threshold
			// bytes survive into the next cycle and accumulate.
			if len(skipped) > 0 {
				if rbErr := c.server.AddUserTrafficSlice(c.runtimeKey, skipped); rbErr != nil {
					log.WithFields(mergeFields(c.logFields(), log.Fields{
						"skipped_count": len(skipped),
						"rollback_err":  rbErr,
					})).Warn("Report ok, but skipped-rollback failed — sub-threshold bytes lost this cycle")
				}
			}
		}
	} else if len(skipped) > 0 {
		// No filtered entries reached the wire (every drained delta was
		// sub-threshold). Roll skipped back so they accumulate toward
		// threshold; otherwise this all-low-traffic cycle would silently
		// lose every byte it drained.
		if rbErr := c.server.AddUserTrafficSlice(c.runtimeKey, skipped); rbErr != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{
				"skipped_count": len(skipped),
				"rollback_err":  rbErr,
			})).Warn("All-sub-threshold cycle: skipped-rollback failed — sub-threshold bytes lost")
		}
	}

	if onlineDevice, err := c.limiter.GetOnlineDevice(); err != nil {
		log.Print(err)
	} else if len(*onlineDevice) > 0 {
		// Online-user threshold (DeviceOnlineMinTraffic*1000 bytes) is
		// independent from TrafficReportThreshold. Walk BOTH filtered
		// and skipped so users we held back via the threshold split
		// still get evaluated for online reporting — the previous
		// pre-split iteration covered them implicitly.
		var result []panel.OnlineUser
		var nocountUID = make(map[int]struct{})
		minOnlineBytes := int64(c.Options.DeviceOnlineMinTraffic * 1000)
		for _, traffic := range userTraffic {
			if traffic.Upload+traffic.Download < minOnlineBytes {
				nocountUID[traffic.UID] = struct{}{}
			}
		}
		for _, traffic := range skipped {
			if traffic.Upload+traffic.Download < minOnlineBytes {
				nocountUID[traffic.UID] = struct{}{}
			}
		}
		for _, online := range *onlineDevice {
			if _, ok := nocountUID[online.UID]; !ok {
				result = append(result, online)
			}
		}
		if err = c.apiClient.ReportNodeOnlineUsers(&result); err != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Info("Report online users failed")
		} else {
			log.WithFields(c.logFields()).Infof("Total %d online users, %d Reported", len(*onlineDevice), len(result))
		}
	}

	CPU, Mem, Disk, Uptime, err := serverstatus.GetSystemInfo()
	if err != nil {
		log.Print(err)
	}
	err = c.apiClient.ReportNodeStatus(
		&panel.NodeStatus{
			CPU:    CPU,
			Mem:    Mem,
			Disk:   Disk,
			Uptime: Uptime,
		})
	if err != nil {
		log.Print(err)
	}

	userTraffic = nil
	return nil
}

// feedDynamicTraffic accumulates bytes from a freshly-reported traffic
// slice into the dynamic-speed-limit counter, keyed by uuid. UID->uuid is
// resolved against the current c.userList; entries whose UID isn't in the
// userList are silently dropped (they belong to a user the panel just
// removed and the periodic refresh hasn't propagated yet — those bytes
// don't need a limit decision).
//
// Called only after a successful ReportUserTraffic so each chunk of bytes
// is counted exactly once: on report failure the rollback path puts the
// bytes back into core's per-user counter, and the next cycle's
// GetUserTrafficSlice will return them again. Feeding here on failure too
// would double-count.
func (c *Controller) feedDynamicTraffic(userTraffic []panel.UserTraffic) {
	if c.traffic == nil || len(userTraffic) == 0 {
		return
	}
	uidToUuid := make(map[int]string, len(c.userList))
	for i := range c.userList {
		uidToUuid[c.userList[i].Id] = c.userList[i].Uuid
	}
	for _, t := range userTraffic {
		uuid, ok := uidToUuid[t.UID]
		if !ok {
			continue
		}
		c.traffic.Add(uuid, t.Upload+t.Download)
	}
}

// reportThreshold returns the panel-configured TrafficReportThreshold
// (bytes) for this controller, defaulting to 0 (filter disabled) if the
// node info or its Basic block hasn't been populated yet — e.g. during
// the first reportUserTrafficTask tick that races with the very first
// successful nodeInfoMonitor.
func (c *Controller) reportThreshold() int64 {
	if c.info == nil || c.info.Common == nil || c.info.Common.Basic == nil {
		return 0
	}
	return c.info.Common.Basic.TrafficReportThreshold
}

// splitUserTrafficByThreshold partitions traffic into entries that
// strictly exceed threshold (`above`) and entries at or below it
// (`belowOrEqual`). Both returned slices are freshly allocated so
// neither aliases the input — callers can keep referencing the
// pre-split data afterwards without worrying about backing-array
// stomp. Mirrors the panel-side predicate at
// queue/logic/traffic/trafficStatisticsLogic.go (drop when
// download+upload <= threshold).
func splitUserTrafficByThreshold(traffic []panel.UserTraffic, threshold int64) (above, belowOrEqual []panel.UserTraffic) {
	for _, t := range traffic {
		if t.Upload+t.Download > threshold {
			above = append(above, t)
		} else {
			belowOrEqual = append(belowOrEqual, t)
		}
	}
	return above, belowOrEqual
}

// userKey returns a stable identity that captures every field the node-side
// limiter cares about. The pipe separator avoids ambiguity between e.g.
// (uuid="u", speed=123) and (uuid="u1", speed=23).
func userKey(u panel.UserInfo) string {
	return fmt.Sprintf("%s|%d|%d", u.Uuid, u.SpeedLimit, u.DeviceLimit)
}

func compareUserList(old, new []panel.UserInfo) (deleted, added []panel.UserInfo) {
	oldMap := make(map[string]int, len(old))
	for i, u := range old {
		oldMap[userKey(u)] = i
	}

	for _, u := range new {
		k := userKey(u)
		if _, exists := oldMap[k]; !exists {
			added = append(added, u)
		} else {
			delete(oldMap, k)
		}
	}

	for _, idx := range oldMap {
		deleted = append(deleted, old[idx])
	}

	return
}
