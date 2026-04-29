package node

import (
	"fmt"
	"time"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/format"
	"github.com/husibo16/yunzes-node/common/task"
	vCore "github.com/husibo16/yunzes-node/core"
	"github.com/husibo16/yunzes-node/limiter"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) startTasks(node *panel.NodeInfo) error {
	c.nodeInfoMonitorPeriodic = &task.Task{
		Interval: node.PullInterval,
		Execute:  c.nodeInfoMonitor,
	}
	c.userReportPeriodic = &task.Task{
		Interval: node.PushInterval,
		Execute:  c.reportUserTrafficTask,
	}
	log.WithFields(c.logFields()).Info("Start monitor node status")
	if err := c.nodeInfoMonitorPeriodic.Start(false); err != nil {
		return fmt.Errorf("start nodeInfoMonitor task: %w", err)
	}
	log.WithFields(c.logFields()).Info("Start report node status")
	if err := c.userReportPeriodic.Start(false); err != nil {
		return fmt.Errorf("start userReport task: %w", err)
	}
	if needsCert(protocolSecurity(node)) && c.CertConfig != nil {
		switch c.CertConfig.CertMode {
		case "none", "", "file", "self":
		default:
			c.renewCertPeriodic = &task.Task{
				Interval: time.Hour * 24,
				Execute:  c.renewCertTask,
			}
			log.WithFields(c.logFields()).Info("Start renew cert")
			if err := c.renewCertPeriodic.Start(true); err != nil {
				return fmt.Errorf("start renewCert task: %w", err)
			}
		}
	}
	if c.LimitConfig.EnableDynamicSpeedLimit {
		c.traffic = make(map[string]int64)
		c.dynamicSpeedLimitPeriodic = &task.Task{
			Interval: time.Duration(c.LimitConfig.DynamicSpeedLimitConfig.Periodic) * time.Second,
			Execute:  c.SpeedChecker,
		}
		log.WithFields(c.logFields()).Info("Start dynamic speed limit")
	}
	return nil
}

// nodeInfoMonitor handles periodic refresh from the panel. It branches on
// whether the node config itself changed (full inbound rebuild) vs. just user
// adds/removes (incremental). Errors are logged and swallowed (return nil) so
// a transient panel hiccup does not stop the periodic task.
//
// The reload path does NOT touch the port registry: the controller's listener
// reservation was claimed once at Start and stays valid for the controller's
// lifetime. If the panel changes the listen port, operators should restart;
// hot-reload of the listen port is out of scope here.
func (c *Controller) nodeInfoMonitor() (err error) {
	newN, err := c.apiClient.GetNodeInfo()
	if err != nil {
		log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Get node info failed")
		return nil
	}
	newU, err := c.apiClient.GetUserList()
	if err != nil {
		log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Get user list failed")
		return nil
	}
	newA, err := c.apiClient.GetUserAlive()
	if err != nil {
		log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Get alive list failed")
		return nil
	}
	if newN != nil {
		c.info = newN
		if newU != nil {
			c.userList = newU
		}
		c.traffic = make(map[string]int64)
		log.WithFields(c.logFields()).Info("Node changed, reload")
		oldRuntimeKey := c.runtimeKey
		oldLogicalTag := c.logicalTag
		err = c.server.DelNode(oldRuntimeKey)
		if err != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Delete node failed")
			return nil
		}

		if len(c.Options.Name) == 0 {
			c.logicalTag = c.buildNodeTag(newN)
			c.runtimeKey = format.RuntimeKey(c.coreType, c.logicalTag)
			limiter.DeleteLimiter(c.coreType, oldLogicalTag)
			c.limiter = limiter.AddLimiter(c.coreType, c.logicalTag, &c.LimitConfig, c.userList, newA)
		}
		if newA != nil {
			c.limiter.AliveList = newA
		}

		if needsCert(protocolSecurity(newN)) && c.CertConfig != nil {
			err = c.requestCert()
			if err != nil {
				log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Request cert failed")
				return nil
			}
		}
		err = c.server.AddNode(c.runtimeKey, newN, c.Options)
		if err != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Add node failed")
			return nil
		}
		_, err = c.server.AddUsers(&vCore.AddUsersParams{
			Tag:      c.runtimeKey,
			Users:    c.userList,
			NodeInfo: newN,
		})
		if err != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Add users failed")
			return nil
		}
		if c.nodeInfoMonitorPeriodic.Interval != newN.PullInterval && newN.PullInterval != 0 {
			c.nodeInfoMonitorPeriodic.Interval = newN.PullInterval
			c.nodeInfoMonitorPeriodic.Close()
			_ = c.nodeInfoMonitorPeriodic.Start(false)
		}
		if c.userReportPeriodic.Interval != newN.PushInterval && newN.PushInterval != 0 {
			c.userReportPeriodic.Interval = newN.PushInterval
			c.userReportPeriodic.Close()
			_ = c.userReportPeriodic.Start(false)
		}
		log.WithFields(c.logFields()).Infof("Added %d new users", len(c.userList))
		return nil
	}
	if newA != nil {
		c.limiter.AliveList = newA
	}
	if len(newU) == 0 {
		return nil
	}
	deleted, added := compareUserList(c.userList, newU)
	if len(deleted) > 0 {
		err = c.server.DelUsers(deleted, c.runtimeKey)
		if err != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Delete users failed")
			return nil
		}
	}
	if len(added) > 0 {
		_, err = c.server.AddUsers(&vCore.AddUsersParams{
			Tag:      c.runtimeKey,
			NodeInfo: c.info,
			Users:    added,
		})
		if err != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Add users failed")
			return nil
		}
	}
	if len(added) > 0 || len(deleted) > 0 {
		c.limiter.UpdateUser(c.runtimeKey, added, deleted)
		if c.LimitConfig.EnableDynamicSpeedLimit {
			for i := range deleted {
				delete(c.traffic, deleted[i].Uuid)
			}
		}
	}
	c.userList = newU
	if len(added)+len(deleted) != 0 {
		log.WithFields(c.logFields()).Infof("%d user deleted, %d user added", len(deleted), len(added))
	}
	return nil
}

func (c *Controller) SpeedChecker() error {
	for u, t := range c.traffic {
		if t >= c.LimitConfig.DynamicSpeedLimitConfig.Traffic {
			err := c.limiter.UpdateDynamicSpeedLimit(c.runtimeKey, u,
				c.LimitConfig.DynamicSpeedLimitConfig.SpeedLimit,
				time.Now().Add(time.Duration(c.LimitConfig.DynamicSpeedLimitConfig.ExpireTime)*time.Minute))
			if err != nil {
				log.WithFields(mergeFields(c.logFields(), log.Fields{"err": err})).Error("Update dynamic speed limit failed")
			}
			delete(c.traffic, u)
		}
	}
	return nil
}

// mergeFields returns a new log.Fields combining base + extra. Both inputs
// are left unchanged. Used to attach an err field on top of the standard
// (logical_tag, core, runtime_key) context without mutating the cached map.
func mergeFields(base, extra log.Fields) log.Fields {
	out := make(log.Fields, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
