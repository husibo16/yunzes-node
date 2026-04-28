package node

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/perfect-panel/ppanel-node/api/panel"
	"github.com/perfect-panel/ppanel-node/conf"
	vCore "github.com/perfect-panel/ppanel-node/core"
	"github.com/sirupsen/logrus"
)

type Node struct {
	controllers []*Controller
}

func New() *Node {
	return &Node{}
}

func (n *Node) Start(nodes []conf.NodeConfig, core vCore.Core) error {
	n.controllers = make([]*Controller, len(nodes))
	for i := range nodes {
		p, err := panel.New(&nodes[i].ApiConfig)
		if err != nil {
			return err
		}
		// Register controller service
		n.controllers[i] = NewController(core, p, &nodes[i].Options)
		err = n.controllers[i].Start()
		if err != nil {
			return fmt.Errorf("start node controller [%s-%s-%d] error: %s",
				nodes[i].ApiConfig.APIHost,
				nodes[i].ApiConfig.NodeType,
				nodes[i].ApiConfig.NodeID,
				err)
		}
	}
	return nil
}

func (n *Node) StartNodes(apiConfig *conf.ServerApiConfig, core vCore.Core) error {
	protocols, basic, err := panel.GetServerNodeConfigs(apiConfig)
	if err != nil {
		return err
	}
	var nodeinfos []*panel.NodeInfo
	n.controllers = make([]*Controller, len(nodeinfos))
	for i, p := range protocols {
		node := &panel.NodeInfo{
			Id:           apiConfig.ServerId,
			Type:         p.Type,
			PushInterval: time.Duration(basic.PushInterval.(int)) * time.Second,
			PullInterval: time.Duration(basic.PullInterval.(int)) * time.Second,
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
					Mode:        p.Mode,
					Extra:       p.Extra,
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
					Mode:        p.Mode,
					Extra:       p.Extra,
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
			return fmt.Errorf("unknown protocol:%s", p.Type)
		}
		nodeinfos = append(nodeinfos, node)

		nodeoptions := &conf.Options{
			DeviceOnlineMinTraffic: 0,
		}
		var coretype string
		switch node.Type {
		case "tuic", "hysteria", "hysteria2", "anytls":
			coretype = "sing"
			nodeoptions.ListenIP = "::"
		case "vmess", "vless", "trojan", "shadowsocks":
			coretype = "xray"
			nodeoptions.ListenIP = "0.0.0.0"
			nodeoptions.XrayOptions = &conf.XrayOptions{
				EnableProxyProtocol: p.EnableProxyProtocol,
			}
		default:
			return fmt.Errorf("unsupported node type: %s", node.Type)
		}
		nodeoptions.Core = coretype
		switch p.Security {
		case "tls":
			nodeoptions.CertConfig = &conf.CertConfig{
				CertMode:         "http", //need to set from panel
				RejectUnknownSni: false,
				CertDomain:       p.SNI,
				CertFile:         fmt.Sprintf("/etc/ppanel-node/%s%d.crt", node.Type, node.Id),
				KeyFile:          fmt.Sprintf("/etc/ppanel-node/%s%d.key", node.Type, node.Id),
			}
		case "reality":
			nodeoptions.CertConfig = &conf.CertConfig{
				CertMode: "none",
			}
		default:
			return fmt.Errorf("unsupported security type: %s", p.Security)
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
				// v.Response contains the last response from the server
				// v.Err contains the original error
				logrus.Error(v.Err)
			}
		})
		client.SetBaseURL(apiConfig.ApiHost)
		client.SetQueryParams(map[string]string{
			"protocol":   node.Type,
			"server_id":  strconv.Itoa(node.Id),
			"secret_key": apiConfig.SecretKey,
		})
		n.controllers[i] = NewController(core, &panel.Client{
			Client:   client,
			Token:    apiConfig.SecretKey,
			APIHost:  apiConfig.ApiHost,
			NodeType: node.Type,
			NodeId:   node.Id,
			UserList: &panel.UserListBody{},
			AliveMap: &panel.AliveMap{},
		}, nodeoptions)
		err = n.controllers[i].Start()
		if err != nil {
			return fmt.Errorf("start node controller [%s-%s-%d] error: %s",
				apiConfig.ApiHost,
				node.Type,
				node.Id,
				err)
		}
	}
	return nil
}

func (n *Node) Close() {
	for _, c := range n.controllers {
		err := c.Close()
		if err != nil {
			panic(err)
		}
	}
	n.controllers = nil
}
