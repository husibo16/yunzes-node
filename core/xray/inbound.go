package xray

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/perfect-panel/ppanel-node/api/panel"
	"github.com/perfect-panel/ppanel-node/conf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	coreConf "github.com/xtls/xray-core/infra/conf"
)

// BuildInbound build Inbound config for different protocol
func buildInbound(option *conf.Options, nodeInfo *panel.NodeInfo, tag string) (*core.InboundHandlerConfig, error) {
	in := &coreConf.InboundDetourConfig{}
	var err error
	var (
		port     uint16
		security string
		network  string
	)

	switch nodeInfo.Common.Protocol {
	case "vless":
		port = uint16(nodeInfo.Common.Vless.Port)
		security = nodeInfo.Common.Vless.Security
		network = nodeInfo.Common.Vless.Network
		err = buildVless(option, nodeInfo, in)
	case "vmess":
		port = uint16(nodeInfo.Common.Vmess.Port)
		security = nodeInfo.Common.Vmess.Security
		network = nodeInfo.Common.Vmess.Network
		err = buildVmess(option, nodeInfo, in)
	case "trojan":
		port = uint16(nodeInfo.Common.Trojan.Port)
		security = nodeInfo.Common.Trojan.Security
		err = buildTrojan(option, nodeInfo, in)
		if nodeInfo.Common.Trojan.Network != "" {
			network = nodeInfo.Common.Trojan.Network
		} else {
			network = "tcp"
		}
	case "shadowsocks":
		port = uint16(nodeInfo.Common.Shadowsocks.Port)
		security = ""
		err = buildShadowsocks(option, nodeInfo, in)
		network = "tcp"
	default:
		return nil, fmt.Errorf("unsupported node type: %s, Only support: vless, vmess, trojan, shadowsocks", nodeInfo.Common.Protocol)
	}

	if err != nil {
		return nil, err
	}
	// Set network protocol
	// Set server port
	in.PortList = &coreConf.PortList{
		Range: []coreConf.PortRange{
			{
				From: uint32(port),
				To:   uint32(port),
			}},
	}
	// Set Listen IP address
	ipAddress := net.ParseAddress(option.ListenIP)
	in.ListenOn = &coreConf.Address{Address: ipAddress}
	// Set SniffingConfig
	sniffingConfig := &coreConf.SniffingConfig{
		Enabled:      true,
		DestOverride: &coreConf.StringList{"http", "tls"},
	}
	if option.XrayOptions.DisableSniffing {
		sniffingConfig.Enabled = false
	}
	in.SniffingConfig = sniffingConfig
	switch network {
	case "tcp":
		if in.StreamSetting.TCPSettings != nil {
			in.StreamSetting.TCPSettings.AcceptProxyProtocol = option.XrayOptions.EnableProxyProtocol
		} else {
			tcpSetting := &coreConf.TCPConfig{
				AcceptProxyProtocol: option.XrayOptions.EnableProxyProtocol,
			} //Enable proxy protocol
			in.StreamSetting.TCPSettings = tcpSetting
		}
	case "ws", "websocket":
		if in.StreamSetting.WSSettings != nil {
			in.StreamSetting.WSSettings.AcceptProxyProtocol = option.XrayOptions.EnableProxyProtocol
		} else {
			in.StreamSetting.WSSettings = &coreConf.WebSocketConfig{
				AcceptProxyProtocol: option.XrayOptions.EnableProxyProtocol,
			} //Enable proxy protocol
		}
	case "httpupgrade":
		if in.StreamSetting.HTTPUPGRADESettings != nil {
			in.StreamSetting.HTTPUPGRADESettings.AcceptProxyProtocol = option.XrayOptions.EnableProxyProtocol
		} else {
			in.StreamSetting.HTTPUPGRADESettings = &coreConf.HttpUpgradeConfig{
				AcceptProxyProtocol: option.XrayOptions.EnableProxyProtocol,
			} //Enable proxy protocol
		}
	default:
		socketConfig := &coreConf.SocketConfig{
			AcceptProxyProtocol: option.XrayOptions.EnableProxyProtocol,
			TFO:                 option.XrayOptions.EnableTFO,
		} //Enable proxy protocol
		in.StreamSetting.SocketSettings = socketConfig
	}
	// Set TLS or Reality settings
	switch security {
	case "tls":
		// Normal tls
		if option.CertConfig == nil {
			return nil, errors.New("the CertConfig is not vail")
		}
		switch option.CertConfig.CertMode {
		case "none", "":
			break // disable
		default:
			in.StreamSetting.Security = "tls"
			in.StreamSetting.TLSSettings = &coreConf.TLSConfig{
				Certs: []*coreConf.TLSCertConfig{
					{
						CertFile:     option.CertConfig.CertFile,
						KeyFile:      option.CertConfig.KeyFile,
						OcspStapling: 3600,
					},
				},
				RejectUnknownSNI: option.CertConfig.RejectUnknownSni,
			}
		}
	case "reality":
		// Reality
		in.StreamSetting.Security = "reality"
		v := nodeInfo.Common.Vless
		dest := v.SecurityConfig.RealityServerAddress
		if dest == "" {
			dest = v.SecurityConfig.SNI
		}
		//xver, _ := strconv.Atoi(v.SecurityConfig.ProxyProtocol)

		d, err := json.Marshal(fmt.Sprintf(
			"%s:%d",
			dest,
			v.SecurityConfig.RealityServerPort))
		if err != nil {
			return nil, fmt.Errorf("marshal reality dest error: %s", err)
		}
		in.StreamSetting.REALITYSettings = &coreConf.REALITYConfig{
			Dest: d,
			//Xver:        uint64(xver),
			ServerNames: []string{v.SecurityConfig.SNI},
			PrivateKey:  v.SecurityConfig.RealityPrivateKey,
			ShortIds:    []string{v.SecurityConfig.RealityShortId},
			Mldsa65Seed: v.SecurityConfig.RealityMldsa65seed,
		}
	default:
		break
	}
	in.Tag = tag
	return in.Build()
}

func buildVless(config *conf.Options, nodeInfo *panel.NodeInfo, inbound *coreConf.InboundDetourConfig) error {
	v := nodeInfo.Common.Vless
	//Set vless
	inbound.Protocol = "vless"
	if config.XrayOptions.EnableFallback {
		// Set fallback
		fallbackConfigs, err := buildVlessFallbacks(config.XrayOptions.FallBackConfigs)
		if err != nil {
			return err
		}
		s, err := json.Marshal(&coreConf.VLessInboundConfig{
			Decryption: "none",
			Fallbacks:  fallbackConfigs,
		})
		if err != nil {
			return fmt.Errorf("marshal vless fallback config error: %s", err)
		}
		inbound.Settings = (*json.RawMessage)(&s)
	} else {
		var err error
		s, err := json.Marshal(&coreConf.VLessInboundConfig{
			Decryption: "none",
		})
		if err != nil {
			return fmt.Errorf("marshal vless config error: %s", err)
		}
		inbound.Settings = (*json.RawMessage)(&s)
	}
	if v.TransportConfig == nil {
		return nil
	}

	t := coreConf.TransportProtocol(v.Network)
	inbound.StreamSetting = &coreConf.StreamConfig{Network: &t}
	switch v.Network {
	case "tcp":
		inbound.StreamSetting.TCPSettings = &coreConf.TCPConfig{}
	case "ws", "websocket":
		inbound.StreamSetting.WSSettings = &coreConf.WebSocketConfig{
			Host: v.TransportConfig.Host,
			Path: v.TransportConfig.Path,
		}
	case "grpc":
		inbound.StreamSetting.GRPCSettings = &coreConf.GRPCConfig{
			ServiceName: v.TransportConfig.ServiceName,
		}
	case "httpupgrade":
		inbound.StreamSetting.HTTPUPGRADESettings = &coreConf.HttpUpgradeConfig{
			Host: v.TransportConfig.Host,
			Path: v.TransportConfig.Path,
		}
	case "splithttp", "xhttp":
		inbound.StreamSetting.SplitHTTPSettings = &coreConf.SplitHTTPConfig{
			Host: v.TransportConfig.Host,
			Path: v.TransportConfig.Path,
			Mode: "auto",
		}
	default:
		return errors.New("the network type is not vail")
	}
	return nil
}

func buildVmess(_ *conf.Options, nodeInfo *panel.NodeInfo, inbound *coreConf.InboundDetourConfig) error {
	v := nodeInfo.Common.Vmess
	// Set vmess
	inbound.Protocol = "vmess"
	var err error
	s, err := json.Marshal(&coreConf.VMessInboundConfig{})
	if err != nil {
		return fmt.Errorf("marshal vmess settings error: %s", err)
	}
	inbound.Settings = (*json.RawMessage)(&s)

	t := coreConf.TransportProtocol(v.Network)
	inbound.StreamSetting = &coreConf.StreamConfig{Network: &t}
	switch v.Network {
	case "tcp":
		inbound.StreamSetting.TCPSettings = &coreConf.TCPConfig{}
	case "ws", "websocket":
		inbound.StreamSetting.WSSettings = &coreConf.WebSocketConfig{
			Host: v.TransportConfig.Host,
			Path: v.TransportConfig.Path,
		}
	case "grpc":
		inbound.StreamSetting.GRPCSettings = &coreConf.GRPCConfig{
			ServiceName: v.TransportConfig.ServiceName,
		}
	case "httpupgrade":
		inbound.StreamSetting.HTTPUPGRADESettings = &coreConf.HttpUpgradeConfig{
			Host: v.TransportConfig.Host,
			Path: v.TransportConfig.Path,
		}
	case "splithttp", "xhttp":
		inbound.StreamSetting.SplitHTTPSettings = &coreConf.SplitHTTPConfig{
			Host: v.TransportConfig.Host,
			Path: v.TransportConfig.Path,
			Mode: "auto",
		}
	default:
		return errors.New("the network type is not vail")
	}
	return nil
}

func buildTrojan(config *conf.Options, nodeInfo *panel.NodeInfo, inbound *coreConf.InboundDetourConfig) error {
	inbound.Protocol = "trojan"
	v := nodeInfo.Common.Trojan
	if config.XrayOptions.EnableFallback {
		// Set fallback
		fallbackConfigs, err := buildTrojanFallbacks(config.XrayOptions.FallBackConfigs)
		if err != nil {
			return err
		}
		s, err := json.Marshal(&coreConf.TrojanServerConfig{
			Fallbacks: fallbackConfigs,
		})
		inbound.Settings = (*json.RawMessage)(&s)
		if err != nil {
			return fmt.Errorf("marshal trojan fallback config error: %s", err)
		}
	} else {
		s := []byte("{}")
		inbound.Settings = (*json.RawMessage)(&s)
	}
	network := v.Network
	if network == "" {
		network = "tcp"
	}
	t := coreConf.TransportProtocol(network)
	inbound.StreamSetting = &coreConf.StreamConfig{Network: &t}
	switch network {
	case "tcp":
		inbound.StreamSetting.TCPSettings = &coreConf.TCPConfig{}
	case "ws", "websocket":
		inbound.StreamSetting.WSSettings = &coreConf.WebSocketConfig{
			Host: v.TransportConfig.Host,
			Path: v.TransportConfig.Path,
		}
	case "grpc":
		inbound.StreamSetting.GRPCSettings = &coreConf.GRPCConfig{
			ServiceName: v.TransportConfig.ServiceName,
		}
	default:
		return errors.New("the network type is not vail")
	}
	return nil
}

func buildShadowsocks(config *conf.Options, nodeInfo *panel.NodeInfo, inbound *coreConf.InboundDetourConfig) error {
	inbound.Protocol = "shadowsocks"
	s := nodeInfo.Common.Shadowsocks
	settings := &coreConf.ShadowsocksServerConfig{
		Cipher: s.Cipher,
	}
	p := make([]byte, 32)
	_, err := rand.Read(p)
	if err != nil {
		return fmt.Errorf("generate random password error: %s", err)
	}
	randomPasswd := hex.EncodeToString(p)
	cipher := s.Cipher
	if s.ServerKey != "" && strings.Contains(s.Cipher, "2022") {
		settings.Password = s.ServerKey
		randomPasswd = base64.StdEncoding.EncodeToString([]byte(randomPasswd))
		cipher = ""
	}
	defaultSSuser := &coreConf.ShadowsocksUserConfig{
		Cipher:   cipher,
		Password: randomPasswd,
	}
	settings.Users = append(settings.Users, defaultSSuser)
	settings.NetworkList = &coreConf.NetworkList{"tcp", "udp"}
	settings.IVCheck = true
	if config.XrayOptions.DisableIVCheck {
		settings.IVCheck = false
	}
	t := coreConf.TransportProtocol("tcp")
	inbound.StreamSetting = &coreConf.StreamConfig{Network: &t}
	sets, err := json.Marshal(settings)
	inbound.Settings = (*json.RawMessage)(&sets)
	if err != nil {
		return fmt.Errorf("marshal shadowsocks settings error: %s", err)
	}
	return nil
}

func buildVlessFallbacks(fallbackConfigs []conf.FallBackConfigForXray) ([]*coreConf.VLessInboundFallback, error) {
	if fallbackConfigs == nil {
		return nil, fmt.Errorf("you must provide FallBackConfigs")
	}
	vlessFallBacks := make([]*coreConf.VLessInboundFallback, len(fallbackConfigs))
	for i, c := range fallbackConfigs {
		if c.Dest == "" {
			return nil, fmt.Errorf("dest is required for fallback fialed")
		}
		var dest json.RawMessage
		dest, err := json.Marshal(c.Dest)
		if err != nil {
			return nil, fmt.Errorf("marshal dest %s config fialed: %s", dest, err)
		}
		vlessFallBacks[i] = &coreConf.VLessInboundFallback{
			Name: c.SNI,
			Alpn: c.Alpn,
			Path: c.Path,
			Dest: dest,
			Xver: c.ProxyProtocolVer,
		}
	}
	return vlessFallBacks, nil
}

func buildTrojanFallbacks(fallbackConfigs []conf.FallBackConfigForXray) ([]*coreConf.TrojanInboundFallback, error) {
	if fallbackConfigs == nil {
		return nil, fmt.Errorf("you must provide FallBackConfigs")
	}

	trojanFallBacks := make([]*coreConf.TrojanInboundFallback, len(fallbackConfigs))
	for i, c := range fallbackConfigs {

		if c.Dest == "" {
			return nil, fmt.Errorf("dest is required for fallback fialed")
		}

		var dest json.RawMessage
		dest, err := json.Marshal(c.Dest)
		if err != nil {
			return nil, fmt.Errorf("marshal dest %s config fialed: %s", dest, err)
		}
		trojanFallBacks[i] = &coreConf.TrojanInboundFallback{
			Name: c.SNI,
			Alpn: c.Alpn,
			Path: c.Path,
			Dest: dest,
			Xver: c.ProxyProtocolVer,
		}
	}
	return trojanFallBacks, nil
}
