package cmd

import (
	"github.com/husibo16/yunzes-node/conf"
	vCore "github.com/husibo16/yunzes-node/core"
	"github.com/husibo16/yunzes-node/node"
	log "github.com/sirupsen/logrus"
)

// nodeRunner is the small closeable surface that reload orchestrates
// against. *node.Node satisfies it via its existing Close() method;
// tests inject a fake to assert teardown order without spinning
// xray-core / sing-box.
type nodeRunner interface {
	Close()
}

// coreFactory builds + starts a vCore.Core from a config. Returns the
// running core OR an error; on error any partially-constructed core
// must already be torn down by the factory itself so the caller does
// not have to guess.
type coreFactory func(*conf.Conf) (vCore.Core, error)

// nodesFactory constructs a node runner against the given core and
// starts the inbounds described in the config. On error the returned
// runner may be nil OR a runner that has already had rollbackControllers
// called (Close is a safe no-op on that state).
type nodesFactory func(*conf.Conf, vCore.Core) (nodeRunner, error)

// runtimeBuilders bundles the two factories the reload orchestrator
// needs. Production wiring uses realRuntimeBuilders; tests inject
// fakes that fail at chosen phases.
type runtimeBuilders struct {
	newCore  coreFactory
	newNodes nodesFactory
}

// realRuntimeBuilders is the production wiring: newCore goes through
// vCore.NewCore + Start; newNodes runs the same StartNodes-vs-Start
// branch the original serverHandle had inline.
func realRuntimeBuilders() runtimeBuilders {
	return runtimeBuilders{
		newCore: func(c *conf.Conf) (vCore.Core, error) {
			vc, err := vCore.NewCore(c.CoresConfig)
			if err != nil {
				return nil, err
			}
			if err := vc.Start(); err != nil {
				_ = vc.Close()
				return nil, err
			}
			return vc, nil
		},
		newNodes: func(c *conf.Conf, core vCore.Core) (nodeRunner, error) {
			ns := node.New()
			var err error
			if c.ApiConfig.ApiHost != "" && c.ApiConfig.ServerId != 0 && c.ApiConfig.SecretKey != "" && len(c.NodeConfig) == 0 {
				err = ns.StartNodes(&c.ApiConfig, core)
			} else {
				err = ns.Start(c.NodeConfig, core)
			}
			return ns, err
		},
	}
}

// reloadOutcome describes what reloadProcess did. Callers use it to
// decide whether to rebind their (vc, nodes) state and whether to
// commit the new *conf.Conf as the active one.
type reloadOutcome int

const (
	// reloadSucceeded means the new config is live; the returned
	// (core, nodes) point at the new runtime.
	reloadSucceeded reloadOutcome = iota
	// reloadKeptOld means a Phase-1 (validation / new-core build)
	// failure was caught before tearing down the old runtime; the
	// returned (core, nodes) ARE the original old, untouched.
	reloadKeptOld
	// reloadRestoredOld means Phase-3 (new-nodes start) failed AFTER
	// old was already torn down; the orchestrator rebuilt the old
	// runtime from oldC and the returned (core, nodes) point at the
	// rebuilt old runtime. Per-user / dynamic state was lost during
	// the rebuild, but the inbound is back.
	reloadRestoredOld
	// reloadOffline means both the new bring-up AND the old-runtime
	// restore failed. Returned (core, nodes) are nil. The process
	// is still alive but has no inbound until the next reload event
	// brings a working config.
	reloadOffline
)

// reloadProcess swaps the runtime from (oldCore, oldNodes) to one
// described by newC. See doc comments on reloadOutcome for what each
// return value combination means.
//
// Layered safety:
//
//   - Phase 1 (no destructive op): build + start new core. Failure
//     here returns reloadKeptOld with (oldCore, oldNodes) unchanged.
//
//   - Phase 2 (commit point): tear down old nodes, then old core. The
//     order matters — closing the nodes releases protocol ports and
//     the listener registry slots before the core itself shuts down.
//
//   - Phase 3 (bring up new): start new nodes against newCore. On
//     failure, tear down newCore and rebuild old from oldC. If the
//     rebuild ALSO fails, return reloadOffline.
func reloadProcess(
	le *log.Entry,
	b runtimeBuilders,
	oldC, newC *conf.Conf,
	oldCore vCore.Core,
	oldNodes nodeRunner,
) (vCore.Core, nodeRunner, reloadOutcome) {

	// Phase 1 — build + start new core.
	newCore, err := b.newCore(newC)
	if err != nil {
		le.WithField("err", err).Error("reload aborted: new core build/start failed; old runtime kept active")
		return oldCore, oldNodes, reloadKeptOld
	}

	// Phase 2 — tear down OLD. From here on, we are committed.
	if oldNodes != nil {
		oldNodes.Close()
	}
	if oldCore != nil {
		if cErr := oldCore.Close(); cErr != nil {
			le.WithField("err", cErr).Warn("close old core errored (continuing reload)")
		}
	}

	// Phase 3 — start new nodes.
	newNodes, err := b.newNodes(newC, newCore)
	if err == nil {
		le.Info("reload succeeded; new runtime active")
		return newCore, newNodes, reloadSucceeded
	}

	le.WithField("err", err).Error("reload failed at new-nodes start; tearing down new and attempting to restore old runtime")
	if newNodes != nil {
		newNodes.Close()
	}
	if cErr := newCore.Close(); cErr != nil {
		le.WithField("err", cErr).Warn("close new core after node failure also errored")
	}

	// Restore old from saved oldC.
	restoredCore, rcErr := b.newCore(oldC)
	if rcErr != nil {
		le.WithField("err", rcErr).Error("RESTORE FAILED: rebuild old core errored; runtime is OFFLINE until next reload")
		return nil, nil, reloadOffline
	}
	restoredNodes, rnErr := b.newNodes(oldC, restoredCore)
	if rnErr != nil {
		if restoredNodes != nil {
			restoredNodes.Close()
		}
		_ = restoredCore.Close()
		le.WithField("err", rnErr).Error("RESTORE FAILED: start restored nodes errored; runtime is OFFLINE until next reload")
		return nil, nil, reloadOffline
	}
	le.Info("reload failed; old runtime restored from snapshot, keep old runtime")
	return restoredCore, restoredNodes, reloadRestoredOld
}
