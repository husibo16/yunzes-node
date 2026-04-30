package node

import (
	"fmt"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/serverstatus"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) reportUserTrafficTask() (err error) {
	userTraffic, _ := c.server.GetUserTrafficSlice(c.runtimeKey, true)
	if len(userTraffic) > 0 {
		err = c.apiClient.ReportUserTraffic(&userTraffic)
		if err != nil {
			c.reportFailures++
			if reportRollbackDecision(c.reportFailures, c.LimitConfig.MaxReportFailureRollbacks) {
				// Bounded-rollback guard: too many consecutive failures.
				// Drop the bytes on the floor so the in-core per-user
				// counter doesn't grow without limit. The panel will
				// undercount this user until reporting recovers.
				log.WithFields(mergeFields(c.logFields(), log.Fields{
					"err":                  err,
					"consecutive_failures": c.reportFailures,
					"dropped_count":        len(userTraffic),
					"rollback_cap":         reportFailureCap(c.LimitConfig.MaxReportFailureRollbacks),
				})).Error("Report failed; rollback cap exceeded — traffic dropped to bound in-core accumulator")
			} else if rbErr := c.server.AddUserTrafficSlice(c.runtimeKey, userTraffic); rbErr != nil {
				log.WithFields(mergeFields(c.logFields(), log.Fields{
					"report_err":           err,
					"rollback_err":         rbErr,
					"consecutive_failures": c.reportFailures,
				})).Error("Report failed AND rollback failed — traffic lost")
			} else {
				log.WithFields(mergeFields(c.logFields(), log.Fields{
					"err":                  err,
					"rolled_back_count":    len(userTraffic),
					"consecutive_failures": c.reportFailures,
				})).Warn("Report failed, traffic rolled back to core for next cycle")
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
		}
	}

	if onlineDevice, err := c.limiter.GetOnlineDevice(); err != nil {
		log.Print(err)
	} else if len(*onlineDevice) > 0 {
		// Only report user has traffic > 100kb to allow ping test
		var result []panel.OnlineUser
		var nocountUID = make(map[int]struct{})
		for _, traffic := range userTraffic {
			total := traffic.Upload + traffic.Download
			if total < int64(c.Options.DeviceOnlineMinTraffic*1000) {
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
