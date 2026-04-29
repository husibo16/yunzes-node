package node

import (
	"errors"
	"fmt"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/task"
	"github.com/husibo16/yunzes-node/conf"
	vCore "github.com/husibo16/yunzes-node/core"
	"github.com/husibo16/yunzes-node/limiter"
	log "github.com/sirupsen/logrus"
)

type Controller struct {
	server                    vCore.Core
	apiClient                 *panel.Client
	tag                       string
	limiter                   *limiter.Limiter
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

// NewController return a Node controller with default parameters.
func NewController(server vCore.Core, api *panel.Client, config *conf.Options) *Controller {
	controller := &Controller{
		server:    server,
		Options:   config,
		apiClient: api,
	}
	return controller
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

// Start brings the controller online. The order is:
//  1. fetch node + users + alive map
//  2. resolve tag
//  3. requestCert if the protocol needs TLS
//  4. add limiter
//  5. AddNode (server inbound)
//  6. AddUsers
//  7. startTasks
//
// Steps 4-7 are guarded by a deferred rollback: any failure undoes the prior
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
		c.tag = c.buildNodeTag(node)
	} else {
		c.tag = c.Options.Name
	}

	security := protocolSecurity(node)
	if needsCert(security) {
		if err = c.requestCert(); err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}

	// Rollback ladder: anything past this point that errors must undo the
	// preceding successful steps.
	c.limiter = limiter.AddLimiter(c.tag, &c.LimitConfig, c.userList, c.aliveMap)
	addedNode := false
	defer func() {
		if err == nil {
			return
		}
		if addedNode {
			if delErr := c.server.DelNode(c.tag); delErr != nil {
				log.WithFields(log.Fields{"tag": c.tag, "err": delErr}).
					Error("rollback DelNode failed")
			}
		}
		limiter.DeleteLimiter(c.tag)
		c.limiter = nil
	}()

	if err = c.server.AddNode(c.tag, node, c.Options); err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	addedNode = true

	added, err := c.server.AddUsers(&vCore.AddUsersParams{
		Tag:      c.tag,
		Users:    c.userList,
		NodeInfo: node,
	})
	if err != nil {
		return fmt.Errorf("add users error: %s", err)
	}
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	c.info = node
	if err = c.startTasks(node); err != nil {
		return fmt.Errorf("start tasks error: %s", err)
	}
	return nil
}

// Close tears down the controller. Safe to call on a half-initialized
// controller (empty tag, nil limiter, nil tasks).
func (c *Controller) Close() error {
	if c.tag != "" {
		limiter.DeleteLimiter(c.tag)
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
	if c.tag == "" || c.server == nil {
		return nil
	}
	if err := c.server.DelNode(c.tag); err != nil {
		return fmt.Errorf("del node error: %s", err)
	}
	return nil
}

func (c *Controller) buildNodeTag(node *panel.NodeInfo) string {
	return fmt.Sprintf("[%s]-%s:%d", c.apiClient.APIHost, node.Type, node.Id)
}
