package sing

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/perfect-panel/ppanel-node/api/panel"
	"github.com/perfect-panel/ppanel-node/conf"
	"github.com/sagernet/sing-box/option"
	F "github.com/sagernet/sing/common/format"
	"github.com/sagernet/sing/common/json/badoption"
)

func getInboundOptions(tag string, info *panel.NodeInfo, c *conf.Options) (option.Inbound, error) {
	addr, err := netip.ParseAddr(c.ListenIP)
	if err != nil {
		return option.Inbound{}, fmt.Errorf("the listen ip not vail")
	}
	var (
		port       uint16
		security   string
		servername string
	)

	switch info.Common.Protocol {
	case "vless":
		port = uint16(info.Common.Vless.Port)
		security = info.Common.Vless.Security
		if security == "tls" || security == "reality" {
			servername = info.Common.Vless.SecurityConfig.SNI
		}
	case "vmess":
		port = uint16(info.Common.Vmess.Port)
		security = info.Common.Vmess.Security
		if security == "tls" || security == "reality" {
			servername = info.Common.Vmess.SecurityConfig.SNI
		}
	case "trojan":
		port = uint16(info.Common.Trojan.Port)
		security = "tls"
		servername = info.Common.Trojan.SecurityConfig.SNI
	case "shadowsocks":
		port = uint16(info.Common.Shadowsocks.Port)
		security = ""
	case "tuic":
		port = uint16(info.Common.Tuic.Port)
		security = "tls"
		servername = info.Common.Tuic.SecurityConfig.SNI
	case "hysteria", "hysteria2":
		port = uint16(info.Common.Hysteria2.Port)
		security = "tls"
		servername = info.Common.Hysteria2.SecurityConfig.SNI
	case "anytls":
		port = uint16(info.Common.AnyTLS.Port)
		security = "tls"
		servername = info.Common.AnyTLS.SecurityConfig.SNI
	default:
		fmt.Println("Unknown protocol:", info.Common.Protocol)
	}

	listen := option.ListenOptions{
		Listen:      (*badoption.Addr)(&addr),
		ListenPort:  port,
		TCPFastOpen: c.SingOptions.TCPFastOpen,
	}
	var multiplex *option.InboundMultiplexOptions
	if c.SingOptions.Multiplex != nil {
		multiplexOption := option.InboundMultiplexOptions{
			Enabled: c.SingOptions.Multiplex.Enabled,
			Padding: c.SingOptions.Multiplex.Padding,
			Brutal: &option.BrutalOptions{
				Enabled:  c.SingOptions.Multiplex.Brutal.Enabled,
				UpMbps:   c.SingOptions.Multiplex.Brutal.UpMbps,
				DownMbps: c.SingOptions.Multiplex.Brutal.DownMbps,
			},
		}
		multiplex = &multiplexOption
	}

	var tls option.InboundTLSOptions
	switch security {
	case "tls":
		if c.CertConfig == nil {
			return option.Inbound{}, fmt.Errorf("the CertConfig is not vail")
		}
		switch c.CertConfig.CertMode {
		case "none", "":
			break // disable
		default:
			tls.Enabled = true
			tls.CertificatePath = c.CertConfig.CertFile
			tls.KeyPath = c.CertConfig.KeyFile
		}
	case "reality":
		tls.Enabled = true
		tls.ServerName = servername
		v := info.Common.Vless.SecurityConfig
		var dest string
		if v.RealityServerAddress != "" {
			dest = v.RealityServerAddress
		} else {
			dest = tls.ServerName
		}

		tls.Reality = &option.InboundRealityOptions{
			Enabled:    true,
			ShortID:    []string{v.RealityShortId},
			PrivateKey: v.RealityPrivateKey,
			Handshake: option.InboundRealityHandshakeOptions{
				ServerOptions: option.ServerOptions{
					Server:     dest,
					ServerPort: uint16(v.RealityServerPort),
				},
			},
		}
	}
	in := option.Inbound{
		Tag: tag,
	}
	switch info.Common.Protocol {
	case "vless":
		v := info.Common.Vless
		t := option.V2RayTransportOptions{
			Type: v.Network,
		}
		switch v.Network {
		case "tcp":
			t.Type = ""
		case "ws", "websocket":
			var (
				path    string
				ed      int
				headers badoption.HTTPHeader
			)
			if v.TransportConfig != nil {
				headers = make(badoption.HTTPHeader)
				headers["Host"] = append(headers["Host"], v.TransportConfig.Host)
				var u *url.URL
				u, err = url.Parse(v.TransportConfig.Path)
				if err != nil {
					return option.Inbound{}, fmt.Errorf("parse path error: %s", err)
				}
				path = u.Path
				ed, _ = strconv.Atoi(u.Query().Get("ed"))
			}
			t.Type = "ws"
			t.WebsocketOptions = option.V2RayWebsocketOptions{
				Path:                path,
				EarlyDataHeaderName: "Sec-WebSocket-Protocol",
				MaxEarlyData:        uint32(ed),
				Headers:             headers,
			}
		case "grpc":
			if v.TransportConfig != nil {
				t.GRPCOptions = option.V2RayGRPCOptions{
					ServiceName: v.TransportConfig.ServiceName,
				}
			}
		case "http2":
			if v.TransportConfig != nil {
				var host badoption.Listable[string]
				host = append(host, v.TransportConfig.Host)
				t.HTTPOptions = option.V2RayHTTPOptions{
					Host: host,
					Path: v.TransportConfig.Path,
				}
			}
		case "httpupgrade":
			if v.TransportConfig != nil {
				t.HTTPUpgradeOptions = option.V2RayHTTPUpgradeOptions{
					Path: v.TransportConfig.Path,
					Host: v.TransportConfig.Host,
				}
			}
		}
		in.Type = "vless"
		in.Options = &option.VLESSInboundOptions{
			ListenOptions: listen,
			InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
				TLS: &tls,
			},
			Transport: &t,
			Multiplex: multiplex,
		}
	case "vmess":
		v := info.Common.Vmess
		t := option.V2RayTransportOptions{
			Type: v.Network,
		}
		switch v.Network {
		case "tcp":
			t.Type = ""
		case "ws", "websocket":
			var (
				path    string
				ed      int
				headers badoption.HTTPHeader
			)
			if v.TransportConfig != nil {
				headers = make(badoption.HTTPHeader)
				headers["Host"] = append(headers["Host"], v.TransportConfig.Host)
				var u *url.URL
				u, err = url.Parse(v.TransportConfig.Path)
				if err != nil {
					return option.Inbound{}, fmt.Errorf("parse path error: %s", err)
				}
				path = u.Path
				ed, _ = strconv.Atoi(u.Query().Get("ed"))
			}
			t.Type = "ws"
			t.WebsocketOptions = option.V2RayWebsocketOptions{
				Path:                path,
				EarlyDataHeaderName: "Sec-WebSocket-Protocol",
				MaxEarlyData:        uint32(ed),
				Headers:             headers,
			}
		case "grpc":
			if v.TransportConfig != nil {
				t.GRPCOptions = option.V2RayGRPCOptions{
					ServiceName: v.TransportConfig.ServiceName,
				}
			}
		case "http2":
			if v.TransportConfig != nil {
				var host badoption.Listable[string]
				host = append(host, v.TransportConfig.Host)
				t.HTTPOptions = option.V2RayHTTPOptions{
					Host: host,
					Path: v.TransportConfig.Path,
				}
			}
		case "httpupgrade":
			if v.TransportConfig != nil {
				t.HTTPUpgradeOptions = option.V2RayHTTPUpgradeOptions{
					Path: v.TransportConfig.Path,
					Host: v.TransportConfig.Host,
				}
			}
		}
		in.Type = "vmess"
		in.Options = &option.VMessInboundOptions{
			ListenOptions: listen,
			InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
				TLS: &tls,
			},
			Transport: &t,
			Multiplex: multiplex,
		}
	case "shadowsocks":
		in.Type = "shadowsocks"
		var keyLength int
		switch info.Common.Shadowsocks.Cipher {
		case "2022-blake3-aes-128-gcm":
			keyLength = 16
		case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
			keyLength = 32
		default:
			keyLength = 16
		}
		ssoption := &option.ShadowsocksInboundOptions{
			ListenOptions: listen,
			Method:        info.Common.Shadowsocks.Cipher,
			Multiplex:     multiplex,
		}
		p := make([]byte, keyLength)
		_, _ = rand.Read(p)
		randomPasswd := string(p)
		if strings.Contains(info.Common.Shadowsocks.Cipher, "2022") {
			ssoption.Password = info.Common.Shadowsocks.ServerKey
			randomPasswd = base64.StdEncoding.EncodeToString([]byte(randomPasswd))
		}
		ssoption.Users = []option.ShadowsocksUser{{
			Password: randomPasswd,
		}}
		in.Options = ssoption
	case "trojan":
		v := info.Common.Trojan
		t := option.V2RayTransportOptions{
			Type: v.Network,
		}
		switch v.Network {
		case "tcp":
			t.Type = ""
		case "ws", "websocket":
			var (
				path    string
				ed      int
				headers badoption.HTTPHeader
			)
			if v.TransportConfig != nil {
				headers = make(badoption.HTTPHeader)
				headers["Host"] = append(headers["Host"], v.TransportConfig.Host)
				var u *url.URL
				u, err = url.Parse(v.TransportConfig.Path)
				if err != nil {
					return option.Inbound{}, fmt.Errorf("parse path error: %s", err)
				}
				path = u.Path
				ed, _ = strconv.Atoi(u.Query().Get("ed"))
			}
			t.Type = "ws"
			t.WebsocketOptions = option.V2RayWebsocketOptions{
				Path:                path,
				EarlyDataHeaderName: "Sec-WebSocket-Protocol",
				MaxEarlyData:        uint32(ed),
				Headers:             headers,
			}
		case "grpc":
			if v.TransportConfig != nil {
				t.GRPCOptions = option.V2RayGRPCOptions{
					ServiceName: v.TransportConfig.ServiceName,
				}
			}
		default:
			t.Type = ""
		}
		in.Type = "trojan"
		trojanoption := &option.TrojanInboundOptions{
			ListenOptions: listen,
			InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
				TLS: &tls,
			},
			Transport: &t,
			Multiplex: multiplex,
		}
		if c.SingOptions.FallBackConfigs != nil {
			// fallback handling
			fallback := c.SingOptions.FallBackConfigs.FallBack
			fallbackPort, err := strconv.Atoi(fallback.ServerPort)
			if err == nil {
				trojanoption.Fallback = &option.ServerOptions{
					Server:     fallback.Server,
					ServerPort: uint16(fallbackPort),
				}
			}
			fallbackForALPNMap := c.SingOptions.FallBackConfigs.FallBackForALPN
			fallbackForALPN := make(map[string]*option.ServerOptions, len(fallbackForALPNMap))
			if err := processFallback(c, fallbackForALPN); err == nil {
				trojanoption.FallbackForALPN = fallbackForALPN
			}
		}
		in.Options = trojanoption
	case "tuic":
		in.Type = "tuic"
		tls.ALPN = append(tls.ALPN, "h3")
		in.Options = &option.TUICInboundOptions{
			ListenOptions: listen,
			InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
				TLS: &tls,
			},
		}
	case "anytls":
		in.Type = "anytls"
		in.Options = &option.AnyTLSInboundOptions{
			ListenOptions: listen,
			InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
				TLS: &tls,
			},
		}
	case "hysteria", "hysteria2":
		in.Type = "hysteria2"
		var obfs *option.Hysteria2Obfs
		if info.Common.Hysteria2.ObfsPassword != "" {
			obfs = &option.Hysteria2Obfs{
				Type:     "salamander",
				Password: info.Common.Hysteria2.ObfsPassword,
			}
		}
		in.Options = &option.Hysteria2InboundOptions{
			ListenOptions: listen,
			Obfs:          obfs,
			InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
				TLS: &tls,
			},
		}
		if info.Common.Hysteria2.UpMbps == 0 && info.Common.Hysteria2.DownMbps == 0 {
			in.Options.(*option.Hysteria2InboundOptions).IgnoreClientBandwidth = true
		} else {
			ino := in.Options.(*option.Hysteria2InboundOptions)
			ino.UpMbps = info.Common.Hysteria2.UpMbps
			ino.DownMbps = info.Common.Hysteria2.DownMbps
			ino.IgnoreClientBandwidth = false
		}
	}
	return in, nil
}

func (b *Sing) AddNode(tag string, info *panel.NodeInfo, config *conf.Options) error {
	c, err := getInboundOptions(tag, info, config)
	if err != nil {
		return err
	}
	in := b.box.Inbound()
	err = in.Create(
		b.ctx,
		b.box.Router(),
		b.logFactory.NewLogger(F.ToString("inbound/", c.Type, "[", tag, "]")),
		tag,
		c.Type,
		c.Options,
	)
	//if err != nil {
	//	return fmt.Errorf("init inbound error： %s", err)
	//}
	//err = in.Start()
	//if err != nil {
	//	return fmt.Errorf("start inbound error: %s", err)
	//}
	//b.inbounds[tag], _ = in.Get(tag)
	//err = b.router.AddInbound(in)
	if err != nil {
		return fmt.Errorf("add inbound error: %s", err)
	}
	return nil
}

func (b *Sing) DelNode(tag string) error {
	in := b.box.Inbound()
	err := in.Remove(tag)
	//err := b.inbounds[tag].Close()
	//if err != nil {
	//	return fmt.Errorf("close inbound error: %s", err)
	//}
	//err = b.router.DelInbound(tag)
	if err != nil {
		return fmt.Errorf("delete inbound error: %s", err)
	}
	return nil
}
