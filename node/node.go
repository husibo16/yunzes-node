package node

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/conf"
	vCore "github.com/husibo16/yunzes-node/core"
	"github.com/sirupsen/logrus"
)

type Node struct {
	controllers []*Controller
	portReg     *portRegistry
}

func New() *Node {
	return &Node{
		portReg: newPortRegistry(),
	}
}

// defaultCoreFor returns the default coreType for a given protocol. Used by
// the local-config Node.Start path to pre-compute coreType before
// controller.Start fetches NodeInfo. The same mapping is duplicated in
// buildServerController for the panel-driven path.
func defaultCoreFor(protocol string) string {
	switch protocol {
	case "tuic", "hysteria", "hysteria2", "anytls":
		return "sing"
	case "vmess", "vless", "trojan", "shadowsocks":
		return "xray"
	}
	return ""
}

// Start brings up controllers from local NodeConfig entries. Controllers are
// appended only after a successful Start; on failure already-started entries
// are closed in reverse order before returning the error.
func (n *Node) Start(nodes []conf.NodeConfig, core vCore.Core) error {
	var started []*Controller
	for i := range nodes {
		p, err := panel.New(&nodes[i].ApiConfig)
		if err != nil {
			rollbackControllers(started)
			return err
		}
		coreType := nodes[i].Options.Core
		if coreType == "" {
			coreType = defaultCoreFor(nodes[i].ApiConfig.NodeType)
		}
		if coreType == "" {
			rollbackControllers(started)
			return fmt.Errorf("cannot infer core for protocol %q", nodes[i].ApiConfig.NodeType)
		}
		ctrl := NewController(coreType, core, p, &nodes[i].Options, n.portReg)
		if err := ctrl.Start(); err != nil {
			rollbackControllers(started)
			return fmt.Errorf("start node controller [%s-%s-%d] error: %s",
				nodes[i].ApiConfig.APIHost,
				nodes[i].ApiConfig.NodeType,
				nodes[i].ApiConfig.NodeID,
				err)
		}
		started = append(started, ctrl)
	}
	n.controllers = started
	return nil
}

// StartNodes brings up controllers from a panel-driven server-protocol list.
// Same accumulation + rollback discipline as Start.
func (n *Node) StartNodes(apiConfig *conf.ServerApiConfig, core vCore.Core) error {
	protocols, basic, err := panel.GetServerNodeConfigs(apiConfig)
	if err != nil {
		return err
	}
	pushI, pullI := resolveIntervals(basic)
	var started []*Controller
	for _, p := range protocols {
		ctrl, err := buildServerController(apiConfig, p, pushI, pullI, core, n.portReg)
		if err != nil {
			rollbackControllers(started)
			return err
		}
		if err := ctrl.Start(); err != nil {
			rollbackControllers(started)
			return fmt.Errorf("start node controller [%s-%s-%d] error: %s",
				apiConfig.ApiHost, p.Type, apiConfig.ServerId, err)
		}
		started = append(started, ctrl)
	}
	n.controllers = started
	return nil
}

// Close tears down all controllers in reverse order of startup. nil entries
// are skipped (Start guarantees the slice contains only successfully started
// controllers, but the guard is kept for defensive use).
func (n *Node) Close() {
	for k := len(n.controllers) - 1; k >= 0; k-- {
		c := n.controllers[k]
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			logrus.WithFields(logrus.Fields{
				"logical_tag": c.logicalTag,
				"core":        c.coreType,
				"runtime_key": c.runtimeKey,
				"err":         err,
			}).Error("close controller failed")
		}
	}
	n.controllers = nil
}

// rollbackControllers tears down a list of started controllers in reverse
// order. Errors are logged but never propagated; the caller is already in a
// failure path returning a different error.
func rollbackControllers(started []*Controller) {
	for k := len(started) - 1; k >= 0; k-- {
		c := started[k]
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			logrus.WithFields(logrus.Fields{
				"logical_tag": c.logicalTag,
				"core":        c.coreType,
				"runtime_key": c.runtimeKey,
				"err":         err,
			}).Error("rollback close failed")
		}
	}
}

// buildServerController assembles a single Controller from one ProtocolConfig
// returned by the panel. It constructs node info, transport options, cert
// config, and the resty client without starting anything.
func buildServerController(apiConfig *conf.ServerApiConfig, p panel.ProtocolConfig, pushI, pullI time.Duration, core vCore.Core, registry *portRegistry) (*Controller, error) {
	node := &panel.NodeInfo{
		Id:           apiConfig.ServerId,
		Type:         p.Type,
		PushInterval: pushI,
		PullInterval: pullI,
		Common: &panel.CommonNode{
			Protocol: p.Type,
		},
	}
	switch p.Type {
	case "vless":
		node.Common.Vless = &panel.VlessNode{
			Port:    p.Port,
			Flow:    p.Flow,
			Network: p.Transport,
			TransportConfig: &panel.TransportConfig{
				Path:        p.Path,
				Host:        p.Host,
				ServiceName: p.ServiceName,
				Mode:        p.XhttpMode,
				Extra:       p.XhttpExtra,
			},
			Security: p.Security,
			SecurityConfig: &panel.SecurityConfig{
				SNI:                  p.SNI,
				RealityServerAddress: p.RealityServerAddr,
				RealityServerPort:    p.RealityServerPort,
				RealityPrivateKey:    p.RealityPrivateKey,
				RealityShortId:       p.RealityShortID,
				RealityMldsa65seed:   p.RealityMldsa65seed,
			},
		}
	case "vmess":
		node.Common.Vmess = &panel.VmessNode{
			Port:    p.Port,
			Network: p.Transport,
			TransportConfig: &panel.TransportConfig{
				Path:        p.Path,
				Host:        p.Host,
				ServiceName: p.ServiceName,
				Mode:        p.XhttpMode,
				Extra:       p.XhttpExtra,
			},
			Security: p.Security,
			SecurityConfig: &panel.SecurityConfig{
				SNI: p.SNI,
			},
		}
	case "trojan":
		node.Common.Trojan = &panel.TrojanNode{
			Port:    p.Port,
			Network: p.Transport,
			TransportConfig: &panel.TransportConfig{
				Path:        p.Path,
				Host:        p.Host,
				ServiceName: p.ServiceName,
			},
			Security: p.Security,
			SecurityConfig: &panel.SecurityConfig{
				SNI: p.SNI,
			},
		}
	case "shadowsocks":
		node.Common.Shadowsocks = &panel.ShadowsocksNode{
			Port:      p.Port,
			Cipher:    p.Cipher,
			ServerKey: p.ServerKey,
		}
	case "tuic":
		node.Common.Tuic = &panel.TuicNode{
			Port: p.Port,
			SecurityConfig: &panel.SecurityConfig{
				SNI: p.SNI,
			},
		}
	case "hysteria", "hysteria2":
		node.Common.Hysteria2 = &panel.Hysteria2Node{
			Port:         p.Port,
			ObfsPassword: p.ObfsPassword,
			UpMbps:       p.UpMbps,
			DownMbps:     p.DownMbps,
			SecurityConfig: &panel.SecurityConfig{
				SNI: p.SNI,
			},
		}
	case "anytls":
		node.Common.AnyTLS = &panel.AnyTLSNode{
			Port: p.Port,
			SecurityConfig: &panel.SecurityConfig{
				SNI: p.SNI,
			},
			PaddingScheme: p.PaddingScheme,
		}
	default:
		return nil, fmt.Errorf("unknown protocol: %s", p.Type)
	}

	nodeoptions := &conf.Options{
		DeviceOnlineMinTraffic: 0,
	}
	coretype := defaultCoreFor(node.Type)
	switch coretype {
	case "sing":
		nodeoptions.ListenIP = "::"
	case "xray":
		nodeoptions.ListenIP = "0.0.0.0"
		nodeoptions.XrayOptions = &conf.XrayOptions{
			EnableProxyProtocol: p.EnableProxyProtocol,
		}
	default:
		return nil, fmt.Errorf("unsupported node type: %s", node.Type)
	}
	nodeoptions.Core = coretype

	// Cert config: TLS protocols pin the cert to /etc/yunzes-node/certs/.
	// Reality / cleartext-shadowsocks / explicit "none" all map to CertMode
	// "none" so the controller skips requestCert. Unknown security values
	// are rejected up front.
	switch p.Security {
	case "tls":
		nodeoptions.CertConfig = resolveCertConfig(p, node.Type, node.Id)
	case "reality", "", "none":
		nodeoptions.CertConfig = &conf.CertConfig{
			CertMode: "none",
		}
	default:
		return nil, fmt.Errorf("unsupported security type: %s", p.Security)
	}

	client := resty.New()
	client.SetRetryCount(3)
	if apiConfig.Timeout > 0 {
		client.SetTimeout(time.Duration(apiConfig.Timeout) * time.Second)
	} else {
		client.SetTimeout(5 * time.Second)
	}
	client.OnError(func(req *resty.Request, err error) {
		var v *resty.ResponseError
		if errors.As(err, &v) {
			logrus.Error(v.Err)
		}
	})
	client.SetBaseURL(apiConfig.ApiHost)
	client.SetQueryParams(map[string]string{
		"protocol":   node.Type,
		"server_id":  strconv.Itoa(node.Id),
		"secret_key": apiConfig.SecretKey,
	})
	return NewController(coretype, core, &panel.Client{
		Client:   client,
		Token:    apiConfig.SecretKey,
		APIHost:  apiConfig.ApiHost,
		NodeType: node.Type,
		NodeId:   node.Id,
		UserList: &panel.UserListBody{},
		AliveMap: &panel.AliveMap{},
	}, nodeoptions, registry), nil
}

// resolveIntervals returns push/pull intervals from BasicConfig with fallbacks.
// JSON decoders default numeric fields to float64, so the original .(int)
// assertion on basic.PushInterval would panic. Also tolerates a nil basic
// (server without the basic field, or pre-fix server build).
func resolveIntervals(basic *panel.BasicConfig) (push, pull time.Duration) {
	const defaultPush = 30 * time.Second
	const defaultPull = 60 * time.Second
	if basic == nil {
		return defaultPush, defaultPull
	}
	push = intervalSec(basic.PushInterval, defaultPush)
	pull = intervalSec(basic.PullInterval, defaultPull)
	return
}

func intervalSec(v any, def time.Duration) time.Duration {
	if v == nil {
		return def
	}
	switch x := v.(type) {
	case int:
		if x <= 0 {
			return def
		}
		return time.Duration(x) * time.Second
	case int64:
		if x <= 0 {
			return def
		}
		return time.Duration(x) * time.Second
	case float64:
		if x <= 0 {
			return def
		}
		return time.Duration(x) * time.Second
	case string:
		n, err := strconv.Atoi(x)
		if err != nil || n <= 0 {
			return def
		}
		return time.Duration(n) * time.Second
	}
	return def
}
