package node

import (
	"errors"
	"fmt"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/format"
	vCore "github.com/husibo16/yunzes-node/core"
	"github.com/husibo16/yunzes-node/limiter"
	log "github.com/sirupsen/logrus"
)

// controllerSnapshot captures the state we need to restore if a reload
// step fails after we've already committed the destructive DelNode call.
// Snapshot fields are immutable for the duration of one reloadNodeConfig
// invocation — the controller's c.* fields are NOT mutated until a
// success commit at the end.
type controllerSnapshot struct {
	runtimeKey string
	logicalTag string
	info       *panel.NodeInfo
	userList   []panel.UserInfo
	aliveMap   map[int]int
	limiter    *limiter.Limiter
}

// reloadNodeConfig drives a panel-reported "node config changed" reload
// without leaving the controller in an offline state when one of the
// downstream operations fails. The previous shape teardown-then-bringup
// plus return-on-error meant that EnsureCertificate / AddNode / AddUsers
// failure left no live inbound until the next pull cycle (often 30-60s).
//
// Layered safety:
//
//   - Layer 1 (pre-validate, no side effects): EnsureCertificate and
//     listenerSpecsFor run BEFORE any destructive call. A failure here
//     returns nil with the old runtime fully intact.
//
//   - Layer 2 (try-with-rollback): DelNode + limiter swap + AddNode +
//     AddUsers under the new identifiers. Any failure after DelNode
//     triggers a rollback that re-installs the old NodeInfo + old
//     limiter + old user list. Rollback itself only logs on failure;
//     bookkeeping always lands on the old state when rollback fails so
//     the next pull cycle attempts the swap again from a known-old
//     position.
//
// Strict "bring up new before tearing down old" cannot work when old
// and new share a listener port (single-process bind constraint), so
// this is the production-grade equivalent: the failure modes that
// previously left the node dead now leave the OLD node serving until
// the operator can fix the new config.
func (c *Controller) reloadNodeConfig(newN *panel.NodeInfo, newU []panel.UserInfo, newA map[int]int) error {
	if newN == nil {
		return errors.New("reloadNodeConfig called with nil NodeInfo")
	}

	old := controllerSnapshot{
		runtimeKey: c.runtimeKey,
		logicalTag: c.logicalTag,
		info:       c.info,
		userList:   c.userList,
		aliveMap:   c.aliveMap,
		limiter:    c.limiter,
	}

	// Compute new identifiers without mutating c.* yet.
	newLogicalTag := old.logicalTag
	newRuntimeKey := old.runtimeKey
	if len(c.Options.Name) == 0 {
		newLogicalTag = c.buildNodeTag(newN)
		newRuntimeKey = format.RuntimeKey(c.coreType, newLogicalTag)
	}

	logFields := func() log.Fields {
		return log.Fields{
			"logical_tag":     old.logicalTag,
			"core":            c.coreType,
			"runtime_key":     old.runtimeKey,
			"new_logical_tag": newLogicalTag,
			"new_runtime_key": newRuntimeKey,
		}
	}

	// Layer 1: pre-validate. No state mutated. Failure here returns nil
	// with old fully intact.
	if needsCert(protocolSecurity(newN)) && c.CertConfig != nil {
		le := log.WithFields(c.logFields())
		if _, err := EnsureCertificate(c.CertConfig, le); err != nil {
			log.WithFields(mergeFields(logFields(), log.Fields{"err": err})).
				Error("reload aborted: ensure cert failed; old node kept running")
			return nil
		}
	}
	if _, err := listenerSpecsFor(newN, c.Options.ListenIP); err != nil {
		log.WithFields(mergeFields(logFields(), log.Fields{"err": err})).
			Error("reload aborted: invalid listener spec on new NodeInfo; old node kept running")
		return nil
	}

	log.WithFields(logFields()).Info("Node changed, reloading")

	// Layer 2: destructive swap. From here on, any failure triggers a
	// rollback that restores the old NodeInfo / users / limiter.
	if err := c.server.DelNode(old.runtimeKey); err != nil {
		// DelNode is supposed to be idempotent; a failure here is anomalous
		// but the old runtime is still as-it-was. Do not attempt rollback
		// since we never finished tearing down.
		log.WithFields(mergeFields(logFields(), log.Fields{"err": err})).
			Error("reload aborted: DelNode(old) failed; old node assumed still active")
		return nil
	}

	// Swap the limiter registry entry (only when the logical tag actually
	// changes — name-pinned controllers reuse the same key). The old
	// limiter pointer is preserved in the snapshot for rollback.
	var newLimiter *limiter.Limiter
	if newLogicalTag != old.logicalTag {
		limiter.DeleteLimiter(c.coreType, old.logicalTag)
		newLimiter = limiter.AddLimiter(c.coreType, newLogicalTag, &c.LimitConfig, newU, newA)
	} else {
		newLimiter = old.limiter
	}

	rollback := func(stage string, cause error) {
		log.WithFields(mergeFields(logFields(), log.Fields{
			"stage": stage,
			"err":   cause,
		})).Error("reload failed; rolling back to old node config")
		// Restore the limiter registry. If we swapped above, drop the
		// freshly-created entry and rebuild the old slot from the old
		// snapshot (state seeded from old.userList + old.aliveMap; per-
		// user dynamic state is lost, same degradation as a successful
		// reload would have caused).
		if newLogicalTag != old.logicalTag {
			limiter.DeleteLimiter(c.coreType, newLogicalTag)
			restored := limiter.AddLimiter(c.coreType, old.logicalTag, &c.LimitConfig, old.userList, old.aliveMap)
			c.limiter = restored
		} else {
			c.limiter = old.limiter
		}
		if rbErr := c.server.AddNode(old.runtimeKey, old.info, c.Options); rbErr != nil {
			log.WithFields(mergeFields(logFields(), log.Fields{
				"stage": stage,
				"err":   rbErr,
			})).Error("ROLLBACK FAILED: AddNode(old) errored; node is OFFLINE until next pull")
			return
		}
		if _, rbErr := c.server.AddUsers(&vCore.AddUsersParams{
			Tag:      old.runtimeKey,
			Users:    old.userList,
			NodeInfo: old.info,
		}); rbErr != nil {
			log.WithFields(mergeFields(logFields(), log.Fields{
				"stage": stage,
				"err":   rbErr,
			})).Error("ROLLBACK PARTIAL: old inbound up but AddUsers(old) failed")
			return
		}
		log.WithFields(logFields()).Info("rollback successful: old node restored")
	}

	if err := c.server.AddNode(newRuntimeKey, newN, c.Options); err != nil {
		rollback("AddNode", err)
		return nil
	}

	if _, err := c.server.AddUsers(&vCore.AddUsersParams{
		Tag:      newRuntimeKey,
		Users:    newU,
		NodeInfo: newN,
	}); err != nil {
		// Tear down the partially-installed new inbound before re-installing
		// the old one. DelNode is idempotent; ignoring its error is safe
		// since rollback's AddNode(old) is what actually restores service.
		if delErr := c.server.DelNode(newRuntimeKey); delErr != nil {
			log.WithFields(mergeFields(logFields(), log.Fields{"err": delErr})).
				Warn("during rollback: DelNode(new) failed; AddNode(old) will still restore")
		}
		rollback("AddUsers", err)
		return nil
	}

	// Success — commit the new state. The order is: identifiers first
	// (so any goroutine that picks up between assignments sees a coherent
	// pair), then accumulators, then alive list.
	c.info = newN
	c.userList = newU
	c.aliveMap = newA
	c.runtimeKey = newRuntimeKey
	c.logicalTag = newLogicalTag
	c.limiter = newLimiter
	c.traffic.Reset()
	if newA != nil {
		c.limiter.AliveList.Replace(newA)
	}

	log.WithFields(c.logFields()).Infof("reload succeeded; %d users active", len(c.userList))
	return nil
}

// adjustPeriodicIntervals is the post-reload bookkeeping that updates
// the pull / push cadence when the panel returned a non-zero new value.
// Extracted so the reload core stays free of *task.Task plumbing.
func (c *Controller) adjustPeriodicIntervals(newN *panel.NodeInfo) {
	if c.nodeInfoMonitorPeriodic != nil &&
		c.nodeInfoMonitorPeriodic.Interval != newN.PullInterval &&
		newN.PullInterval != 0 {
		c.nodeInfoMonitorPeriodic.Interval = newN.PullInterval
		c.nodeInfoMonitorPeriodic.Close()
		_ = c.nodeInfoMonitorPeriodic.Start(false)
	}
	if c.userReportPeriodic != nil &&
		c.userReportPeriodic.Interval != newN.PushInterval &&
		newN.PushInterval != 0 {
		c.userReportPeriodic.Interval = newN.PushInterval
		c.userReportPeriodic.Close()
		_ = c.userReportPeriodic.Start(false)
	}
}

// nodeReloadDebugString is a small helper for log lines that want to
// describe the requested transition without leaking pointer formatting.
// Used by tests.
func (s controllerSnapshot) String() string {
	return fmt.Sprintf("snapshot(rk=%q tag=%q users=%d)", s.runtimeKey, s.logicalTag, len(s.userList))
}
