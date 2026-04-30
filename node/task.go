package node

import (
	"fmt"
	"time"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/task"
	vCore "github.com/husibo16/yunzes-node/core"
	log "github.com/sirupsen/logrus"
)

// renewCertTask is the periodic hook scheduled by startTasks. It defers all
// lifecycle decisions to EnsureCertificate, which stat+parses the on-disk
// cert and decides whether to issue, renew, reuse, or reissue.
func (c *Controller) renewCertTask() error {
	le := log.WithFields(c.logFields())
	if _, err := EnsureCertificate(c.CertConfig, le); err != nil {
		le.WithField("err", err).Info("ensure cert (periodic) failed")
	}
	return nil
}

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
		if c.LimitConfig.DynamicSpeedLimitConfig == nil {
			return fmt.Errorf("EnableDynamicSpeedLimit set but DynamicSpeedLimitConfig is nil")
		}
		c.traffic = newTrafficStore()
		c.dynamicSpeedLimitPeriodic = &task.Task{
			Interval: time.Duration(c.LimitConfig.DynamicSpeedLimitConfig.Periodic) * time.Second,
			Execute:  c.SpeedChecker,
		}
		log.WithFields(c.logFields()).Info("Start dynamic speed limit")
		if err := c.dynamicSpeedLimitPeriodic.Start(false); err != nil {
			return fmt.Errorf("start dynamicSpeedLimit task: %w", err)
		}
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
		// Configuration changed. Hand off to reloadNodeConfig which
		// snapshots old state, pre-validates the new one, and on any
		// downstream failure rolls back to the old NodeInfo + user list
		// + limiter rather than leaving the node offline until the next
		// pull cycle.
		if reloadErr := c.reloadNodeConfig(newN, newU, newA); reloadErr != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{"err": reloadErr})).
				Error("reload returned unexpected error")
			return nil
		}
		c.adjustPeriodicIntervals(newN)
		return nil
	}
	if newA != nil {
		c.limiter.AliveList.Replace(newA)
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
				c.traffic.Delete(deleted[i].Uuid)
			}
		}
	}
	c.userList = newU
	if len(added)+len(deleted) != 0 {
		log.WithFields(c.logFields()).Infof("%d user deleted, %d user added", len(deleted), len(added))
	}
	return nil
}

// SpeedChecker is the dynamicSpeedLimitPeriodic body. It atomically pulls
// every uuid that crossed the configured byte threshold out of c.traffic
// and asks the limiter to flip them into dynamic-limit mode for the
// configured ExpireTime window. Drain clears the entries under the same
// lock that reads them, so any bytes Add()ed concurrently land in a fresh
// counter for the next cycle (no double-trigger, no lost bytes).
func (c *Controller) SpeedChecker() error {
	cfg := c.LimitConfig.DynamicSpeedLimitConfig
	if cfg == nil {
		return nil
	}
	expire := time.Now().Add(time.Duration(cfg.ExpireTime) * time.Minute)
	for _, uuid := range c.traffic.Drain(cfg.Traffic) {
		if err := c.limiter.UpdateDynamicSpeedLimit(c.runtimeKey, uuid, cfg.SpeedLimit, expire); err != nil {
			log.WithFields(mergeFields(c.logFields(), log.Fields{
				"uuid": uuid,
				"err":  err,
			})).Error("Update dynamic speed limit failed")
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
