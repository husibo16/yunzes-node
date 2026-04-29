package node

import (
	"errors"
	"fmt"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/format"
	"github.com/husibo16/yunzes-node/common/task"
	"github.com/husibo16/yunzes-node/conf"
	vCore "github.com/husibo16/yunzes-node/core"
	"github.com/husibo16/yunzes-node/limiter"
	log "github.com/sirupsen/logrus"
)

// Controller wires one panel-driven node into one core (xray or sing).
//
// The three identifiers carry distinct contracts:
//
//   - coreType: "xray" | "sing". Decides which adapter receives AddNode etc.
//     Stamped on every log line.
//
//   - logicalTag: human-facing tag built by buildNodeTag (or pinned via
//     Options.Name). This is what we report up to the panel server (traffic
//     reports, online users) and what shows up in operator-facing logs.
//
//   - runtimeKey: coreType + "|" + logicalTag. The internal-only handle that
//     keys the limiter map AND is registered as the inbound tag inside
//     xray-core / sing-box. Hot-path lookups (sing hook, xray dispatcher)
//     pass sessionInbound.Tag straight in — they never split this string.
type Controller struct {
	server                    vCore.Core
	apiClient                 *panel.Client
	coreType                  string
	logicalTag                string
	runtimeKey                string
	limiter                   *limiter.Limiter
	portRegistry              *portRegistry
	traffic                   map[string]int64
	userList                  []panel.UserInfo
	aliveMap                  map[int]int
	info                      *panel.NodeInfo
	nodeInfoMonitorPeriodic   *task.Task
	userReportPeriodic        *task.Task
	renewCertPeriodic         *task.Task
	dynamicSpeedLimitPeriodic *task.Task
	*conf.Options
}

// NewController constructs a Controller bound to a specific coreType. The
// portRegistry may be nil for legacy / test paths; production callers
// (Node.Start, Node.StartNodes) always pass one so port-conflict detection
// is uniform across both startup paths.
func NewController(coreType string, server vCore.Core, api *panel.Client, config *conf.Options, registry *portRegistry) *Controller {
	return &Controller{
		server:       server,
		Options:      config,
		apiClient:    api,
		coreType:     coreType,
		portRegistry: registry,
	}
}

// protocolSecurity returns the security mode that applies to a given node's
// inbound. "tls" requires an X.509 cert (file/ACME/self). "reality" uses xray
// reality keys (no cert needed). "" means cleartext (e.g. shadowsocks).
func protocolSecurity(node *panel.NodeInfo) string {
	switch node.Common.Protocol {
	case "vless":
		if node.Common.Vless != nil {
			return node.Common.Vless.Security
		}
	case "vmess":
		if node.Common.Vmess != nil {
			return node.Common.Vmess.Security
		}
	case "trojan":
		if node.Common.Trojan != nil {
			return node.Common.Trojan.Security
		}
	case "tuic", "hysteria", "hysteria2", "anytls":
		return "tls"
	case "shadowsocks":
		return ""
	}
	return ""
}

// needsCert reports whether a security mode requires the controller to drive
// the X.509 cert path (requestCert + renewCertTask). Only "tls" does;
// "reality" carries its own keypair, "" / "none" are cleartext.
func needsCert(security string) bool {
	return security == "tls"
}

// logFields returns the standard 3-field structured log context every
// controller log line should carry.
func (c *Controller) logFields() log.Fields {
	return log.Fields{
		"logical_tag": c.logicalTag,
		"core":        c.coreType,
		"runtime_key": c.runtimeKey,
	}
}

// startupLogFields adds protocol/server-id/listen-addr/port/transport context
// for events that happen at controller start where the NodeInfo is in scope.
func (c *Controller) startupLogFields(node *panel.NodeInfo) log.Fields {
	f := c.logFields()
	f["protocol"] = node.Common.Protocol
	f["server_id"] = node.Id
	f["listen_addr"] = normalizeListenAddr(c.Options.ListenIP)
	if port, err := protocolPort(node); err == nil {
		f["port"] = port
	}
	if transports := protocolTransports(node.Common.Protocol); len(transports) > 0 {
		f["network"] = transports
	}
	return f
}

// Start brings the controller online. The order is:
//
//  1. fetch node + users + alive map
//  2. resolve logicalTag + runtimeKey
//  3. reserve listener(s) in the port registry (fail-fast on conflict)
//  4. requestCert if the protocol needs TLS
//  5. add limiter
//  6. AddNode (server inbound, registered under runtimeKey)
//  7. AddUsers
//  8. startTasks
//
// Steps 3-8 are guarded by deferred rollbacks: any failure undoes the prior
// successful steps so we never leave a half-built controller behind.
func (c *Controller) Start() (err error) {
	node, err := c.apiClient.GetNodeInfo()
	if err != nil {
		return fmt.Errorf("get node info error: %s", err)
	}
	c.userList, err = c.apiClient.GetUserList()
	if err != nil {
		return fmt.Errorf("get user list error: %s", err)
	}
	if len(c.userList) == 0 {
		return errors.New("add users error: not have any user")
	}
	c.aliveMap, err = c.apiClient.GetUserAlive()
	if err != nil {
		return fmt.Errorf("failed to get user alive list: %s", err)
	}
	if len(c.Options.Name) == 0 {
		c.logicalTag = c.buildNodeTag(node)
	} else {
		c.logicalTag = c.Options.Name
	}
	c.runtimeKey = format.RuntimeKey(c.coreType, c.logicalTag)

	// Port registry — fail-fast before any cert / limiter / inbound work.
	if c.portRegistry != nil {
		specs, specErr := listenerSpecsFor(node, c.Options.ListenIP)
		if specErr != nil {
			return fmt.Errorf("listener specs: %s", specErr)
		}
		if err = c.portRegistry.reserve(c.runtimeKey, c.logicalTag, specs); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				c.portRegistry.release(c.runtimeKey)
			}
		}()
	}

	security := protocolSecurity(node)
	if needsCert(security) {
		if err = c.requestCert(); err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}

	// Rollback ladder: anything past this point that errors must undo the
	// preceding successful steps.
	c.limiter = limiter.AddLimiter(c.coreType, c.logicalTag, &c.LimitConfig, c.userList, c.aliveMap)
	addedNode := false
	defer func() {
		if err == nil {
			return
		}
		if addedNode {
			if delErr := c.server.DelNode(c.runtimeKey); delErr != nil {
				log.WithFields(log.Fields{
					"logical_tag": c.logicalTag,
					"core":        c.coreType,
					"runtime_key": c.runtimeKey,
					"err":         delErr,
				}).Error("rollback DelNode failed")
			}
		}
		limiter.DeleteLimiter(c.coreType, c.logicalTag)
		c.limiter = nil
	}()

	log.WithFields(c.startupLogFields(node)).Info("Adding node inbound")
	if err = c.server.AddNode(c.runtimeKey, node, c.Options); err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	addedNode = true

	added, err := c.server.AddUsers(&vCore.AddUsersParams{
		Tag:      c.runtimeKey,
		Users:    c.userList,
		NodeInfo: node,
	})
	if err != nil {
		return fmt.Errorf("add users error: %s", err)
	}
	log.WithFields(c.logFields()).Infof("Added %d new users", added)
	c.info = node
	if err = c.startTasks(node); err != nil {
		return fmt.Errorf("start tasks error: %s", err)
	}
	return nil
}

// Close tears down the controller. Safe to call on a half-initialized
// controller (empty runtimeKey, nil server, nil tasks).
func (c *Controller) Close() error {
	if c.runtimeKey != "" {
		limiter.DeleteLimiter(c.coreType, c.logicalTag)
	}
	if c.nodeInfoMonitorPeriodic != nil {
		c.nodeInfoMonitorPeriodic.Close()
	}
	if c.userReportPeriodic != nil {
		c.userReportPeriodic.Close()
	}
	if c.renewCertPeriodic != nil {
		c.renewCertPeriodic.Close()
	}
	if c.dynamicSpeedLimitPeriodic != nil {
		c.dynamicSpeedLimitPeriodic.Close()
	}
	if c.runtimeKey == "" || c.server == nil {
		return nil
	}
	delErr := c.server.DelNode(c.runtimeKey)
	if c.portRegistry != nil {
		c.portRegistry.release(c.runtimeKey)
	}
	if delErr != nil {
		return fmt.Errorf("del node error: %s", delErr)
	}
	return nil
}

func (c *Controller) buildNodeTag(node *panel.NodeInfo) string {
	return fmt.Sprintf("[%s]-%s:%d", c.apiClient.APIHost, node.Type, node.Id)
}
