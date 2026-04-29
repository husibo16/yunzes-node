package xray

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/format"
	"github.com/husibo16/yunzes-node/conf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	coreConf "github.com/xtls/xray-core/infra/conf"
)

// BuildInbound build Inbound config for different protocol.
//
// Hardened against nil panel payloads: every dereference of NodeInfo.Common
// or its protocol-specific sub-struct is gated by a nil-check that turns
// missing fields into a returned error rather than a runtime panic.
// Defensive guards also exist around in.StreamSetting before the network
// switch — a buggy build* helper that forgets to allocate it must not crash
// the daemon.
func buildInbound(option *conf.Options, nodeInfo *panel.NodeInfo, tag string) (*core.InboundHandlerConfig, error) {
	if nodeInfo == nil || nodeInfo.Common == nil {
		return nil, errors.New("buildInbound: nodeInfo or Common is nil")
	}
	if option == nil || option.XrayOptions == nil {
		return nil, errors.New("buildInbound: option or XrayOptions is nil")
	}

	in := &coreConf.InboundDetourConfig{}
	var err error
	var (
		port     uint16
		security string
		network  string
	)

	switch nodeInfo.Common.Protocol {
	case "vless":
		if nodeInfo.Common.Vless == nil {
			return nil, errors.New("buildInbound: vless protocol but Common.Vless is nil")
		}
		port = uint16(nodeInfo.Common.Vless.Port)
		security = nodeInfo.Common.Vless.Security
		network = nodeInfo.Common.Vless.Network
		err = buildVless(option, nodeInfo, in)
	case "vmess":
		if nodeInfo.Common.Vmess == nil {
			return nil, errors.New("buildInbound: vmess protocol but Common.Vmess is nil")
		}
		port = uint16(nodeInfo.Common.Vmess.Port)
		security = nodeInfo.Common.Vmess.Security
		network = nodeInfo.Common.Vmess.Network
		err = buildVmess(option, nodeInfo, in)
	case "trojan":
		if nodeInfo.Common.Trojan == nil {
			return nil, errors.New("buildInbound: trojan protocol but Common.Trojan is nil")
		}
		port = uint16(nodeInfo.Common.Trojan.Port)
		security = nodeInfo.Common.Trojan.Security
		err = buildTrojan(option, nodeInfo, in)
		if nodeInfo.Common.Trojan.Network != "" {
			network = nodeInfo.Common.Trojan.Network
		} else {
			network = "tcp"
		}
	case "shadowsocks":
		if nodeInfo.Common.Shadowsocks == nil {
			return nil, errors.New("buildInbound: shadowsocks protocol but Common.Shadowsocks is nil")
		}
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
	if network == "" {
		network = "tcp"
	}

	in.PortList = &coreConf.PortList{
		Range: []coreConf.PortRange{{From: uint32(port), To: uint32(port)}},
	}
	ipAddress := net.ParseAddress(option.ListenIP)
	in.ListenOn = &coreConf.Address{Address: ipAddress}
	sniffingConfig := &coreConf.SniffingConfig{
		Enabled:      true,
		DestOverride: &coreConf.StringList{"http", "tls"},
	}
	if option.XrayOptions.DisableSniffing {
		sniffingConfig.Enabled = false
	}
	in.SniffingConfig = sniffingConfig

	// Defensive: a build* helper may not have allocated StreamSetting
	// (e.g. vless when TransportConfig is nil). The network switch below
	// dereferences it, so guarantee it's non-nil here.
	if in.StreamSetting == nil {
		t := coreConf.TransportProtocol(network)
		in.StreamSetting = &coreConf.StreamConfig{Network: &t}
	}

	switch network {
	case "tcp":
		if in.StreamSetting.TCPSettings == nil {
			in.StreamSetting.TCPSettings = &coreConf.TCPConfig{
				AcceptProxyProtocol: option.XrayOptions.EnableProxyProtocol,
			}
		} else {
			in.StreamSetting.TCPSettings.AcceptProxyProtocol = option.XrayOptions.EnableProxyProtocol
		}
	case "ws", "websocket":
		if in.StreamSetting.WSSettings == nil {
			in.StreamSetting.WSSettings = &coreConf.WebSocketConfig{
				AcceptProxyProtocol: option.XrayOptions.EnableProxyProtocol,
			}
		} else {
			in.StreamSetting.WSSettings.AcceptProxyProtocol = option.XrayOptions.EnableProxyProtocol
		}
	case "httpupgrade":
		if in.StreamSetting.HTTPUPGRADESettings == nil {
			in.StreamSetting.HTTPUPGRADESettings = &coreConf.HttpUpgradeConfig{
				AcceptProxyProtocol: option.XrayOptions.EnableProxyProtocol,
			}
		} else {
			in.StreamSetting.HTTPUPGRADESettings.AcceptProxyProtocol = option.XrayOptions.EnableProxyProtocol
		}
	default:
		in.StreamSetting.SocketSettings = &coreConf.SocketConfig{
			AcceptProxyProtocol: option.XrayOptions.EnableProxyProtocol,
			TFO:                 option.XrayOptions.EnableTFO,
		}
	}

	switch security {
	case "tls":
		if option.CertConfig == nil {
			return nil, errors.New("invalid tls config: missing CertConfig")
		}
		switch option.CertConfig.CertMode {
		case "none", "":
			// disable
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
		// REALITY only applies to vless today; the protocol switch above
		// already nil-checked Common.Vless when we entered the vless arm,
		// but be explicit here so a future caller of this branch from
		// another protocol doesn't surprise-panic.
		if nodeInfo.Common.Vless == nil {
			return nil, errors.New("invalid reality config: not a vless node")
		}
		realitySettings, rErr := buildRealitySettings(nodeInfo.Common.Vless)
		if rErr != nil {
			return nil, rErr
		}
		in.StreamSetting.Security = "reality"
		in.StreamSetting.REALITYSettings = realitySettings
	default:
		// cleartext / unknown — leave StreamSetting.Security empty.
	}
	in.Tag = tag
	return in.Build()
}

// buildRealitySettings validates the reality-relevant SecurityConfig fields
// and produces a coreConf.REALITYConfig. Every required field is checked
// before any dereference so a panel response missing security_config (or any
// of its sub-fields) returns a precise error instead of a nil panic.
func buildRealitySettings(v *panel.VlessNode) (*coreConf.REALITYConfig, error) {
	if v.SecurityConfig == nil {
		return nil, errors.New("invalid reality config: missing security_config")
	}
	sc := v.SecurityConfig
	if sc.SNI == "" {
		return nil, errors.New("invalid reality config: missing sni / server_name")
	}
	if sc.RealityPrivateKey == "" {
		return nil, errors.New("invalid reality config: missing reality_private_key")
	}
	dest := sc.RealityServerAddress
	if dest == "" {
		dest = sc.SNI
	}
	if dest == "" {
		return nil, errors.New("invalid reality config: missing reality_server_addr (and sni fallback)")
	}
	if sc.RealityServerPort <= 0 || sc.RealityServerPort > 65535 {
		return nil, fmt.Errorf("invalid reality config: bad reality_server_port=%d", sc.RealityServerPort)
	}
	d, err := json.Marshal(fmt.Sprintf("%s:%d", dest, sc.RealityServerPort))
	if err != nil {
		return nil, fmt.Errorf("invalid reality config: marshal dest: %s", err)
	}
	// xray-core accepts an empty short-id (means the server tolerates any
	// short-id from clients). Pass through whatever the panel said —
	// validation of crypto correctness is xray-core's responsibility.
	return &coreConf.REALITYConfig{
		Dest:        d,
		ServerNames: []string{sc.SNI},
		PrivateKey:  sc.RealityPrivateKey,
		ShortIds:    []string{sc.RealityShortId},
		Mldsa65Seed: sc.RealityMldsa65seed,
	}, nil
}

func buildVless(config *conf.Options, nodeInfo *panel.NodeInfo, inbound *coreConf.InboundDetourConfig) error {
	v := nodeInfo.Common.Vless
	if v == nil {
		return errors.New("buildVless: Common.Vless is nil")
	}
	if config.XrayOptions == nil {
		return errors.New("buildVless: XrayOptions is nil")
	}
	inbound.Protocol = "vless"
	if config.XrayOptions.EnableFallback {
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
		s, err := json.Marshal(&coreConf.VLessInboundConfig{Decryption: "none"})
		if err != nil {
			return fmt.Errorf("marshal vless config error: %s", err)
		}
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
		// tcp does not need TransportConfig; nil is fine.
		inbound.StreamSetting.TCPSettings = &coreConf.TCPConfig{}
	case "ws", "websocket":
		host, path := transportHostPath(v.TransportConfig)
		inbound.StreamSetting.WSSettings = &coreConf.WebSocketConfig{
			Host: host,
			Path: path,
		}
	case "grpc":
		serviceName := ""
		if v.TransportConfig != nil {
			serviceName = v.TransportConfig.ServiceName
		}
		inbound.StreamSetting.GRPCSettings = &coreConf.GRPCConfig{
			ServiceName: serviceName,
		}
	case "httpupgrade":
		host, path := transportHostPath(v.TransportConfig)
		inbound.StreamSetting.HTTPUPGRADESettings = &coreConf.HttpUpgradeConfig{
			Host: host,
			Path: path,
		}
	case "splithttp", "xhttp":
		host, path := transportHostPath(v.TransportConfig)
		inbound.StreamSetting.SplitHTTPSettings = &coreConf.SplitHTTPConfig{
			Host: host,
			Path: path,
			Mode: "auto",
		}
	default:
		return fmt.Errorf("vless: unsupported network type %q", network)
	}
	return nil
}

func buildVmess(_ *conf.Options, nodeInfo *panel.NodeInfo, inbound *coreConf.InboundDetourConfig) error {
	v := nodeInfo.Common.Vmess
	if v == nil {
		return errors.New("buildVmess: Common.Vmess is nil")
	}
	inbound.Protocol = "vmess"
	s, err := json.Marshal(&coreConf.VMessInboundConfig{})
	if err != nil {
		return fmt.Errorf("marshal vmess settings error: %s", err)
	}
	inbound.Settings = (*json.RawMessage)(&s)

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
		host, path := transportHostPath(v.TransportConfig)
		inbound.StreamSetting.WSSettings = &coreConf.WebSocketConfig{
			Host: host,
			Path: path,
		}
	case "grpc":
		serviceName := ""
		if v.TransportConfig != nil {
			serviceName = v.TransportConfig.ServiceName
		}
		inbound.StreamSetting.GRPCSettings = &coreConf.GRPCConfig{
			ServiceName: serviceName,
		}
	case "httpupgrade":
		host, path := transportHostPath(v.TransportConfig)
		inbound.StreamSetting.HTTPUPGRADESettings = &coreConf.HttpUpgradeConfig{
			Host: host,
			Path: path,
		}
	case "splithttp", "xhttp":
		host, path := transportHostPath(v.TransportConfig)
		inbound.StreamSetting.SplitHTTPSettings = &coreConf.SplitHTTPConfig{
			Host: host,
			Path: path,
			Mode: "auto",
		}
	default:
		return fmt.Errorf("vmess: unsupported network type %q", network)
	}
	return nil
}

func buildTrojan(config *conf.Options, nodeInfo *panel.NodeInfo, inbound *coreConf.InboundDetourConfig) error {
	v := nodeInfo.Common.Trojan
	if v == nil {
		return errors.New("buildTrojan: Common.Trojan is nil")
	}
	if config.XrayOptions == nil {
		return errors.New("buildTrojan: XrayOptions is nil")
	}
	inbound.Protocol = "trojan"
	if config.XrayOptions.EnableFallback {
		fallbackConfigs, err := buildTrojanFallbacks(config.XrayOptions.FallBackConfigs)
		if err != nil {
			return err
		}
		s, err := json.Marshal(&coreConf.TrojanServerConfig{
			Fallbacks: fallbackConfigs,
		})
		if err != nil {
			return fmt.Errorf("marshal trojan fallback config error: %s", err)
		}
		inbound.Settings = (*json.RawMessage)(&s)
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
		host, path := transportHostPath(v.TransportConfig)
		inbound.StreamSetting.WSSettings = &coreConf.WebSocketConfig{
			Host: host,
			Path: path,
		}
	case "grpc":
		serviceName := ""
		if v.TransportConfig != nil {
			serviceName = v.TransportConfig.ServiceName
		}
		inbound.StreamSetting.GRPCSettings = &coreConf.GRPCConfig{
			ServiceName: serviceName,
		}
	default:
		return fmt.Errorf("trojan: unsupported network type %q", network)
	}
	return nil
}

func buildShadowsocks(config *conf.Options, nodeInfo *panel.NodeInfo, inbound *coreConf.InboundDetourConfig) error {
	s := nodeInfo.Common.Shadowsocks
	if s == nil {
		return errors.New("buildShadowsocks: Common.Shadowsocks is nil")
	}
	if config.XrayOptions == nil {
		return errors.New("buildShadowsocks: XrayOptions is nil")
	}
	inbound.Protocol = "shadowsocks"
	if err := format.ValidateShadowsocksCipher(s.Cipher, s.ServerKey); err != nil {
		return err
	}
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
	if err != nil {
		return fmt.Errorf("marshal shadowsocks settings error: %s", err)
	}
	inbound.Settings = (*json.RawMessage)(&sets)
	return nil
}

// transportHostPath returns Host / Path from a possibly-nil TransportConfig.
// Centralizes the nil-check so per-protocol builders don't repeat it.
func transportHostPath(tc *panel.TransportConfig) (host, path string) {
	if tc == nil {
		return "", ""
	}
	return tc.Host, tc.Path
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
