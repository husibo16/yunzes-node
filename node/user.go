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
			// Rollback so the next cycle re-reports.
			if rbErr := c.server.AddUserTrafficSlice(c.runtimeKey, userTraffic); rbErr != nil {
				log.WithFields(mergeFields(c.logFields(), log.Fields{
					"report_err":   err,
					"rollback_err": rbErr,
				})).Error("Report failed AND rollback failed — traffic lost")
			} else {
				log.WithFields(mergeFields(c.logFields(), log.Fields{
					"err":               err,
					"rolled_back_count": len(userTraffic),
				})).Warn("Report failed, traffic rolled back to core for next cycle")
			}
		} else {
			log.WithFields(c.logFields()).Infof("Report %d users traffic", len(userTraffic))
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
