package xray

import (
	"strings"
	"testing"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/conf"
)

// xrayOpts builds a minimal valid Options{XrayOptions} for buildInbound
// invocations under test. Tests that need to exercise individual fields
// override after construction.
func xrayOpts() *conf.Options {
	return &conf.Options{
		ListenIP:    "0.0.0.0",
		XrayOptions: &conf.XrayOptions{},
		CertConfig:  &conf.CertConfig{CertMode: "none"},
	}
}

// callBuildInbound is a panic-trapping helper. If buildInbound ever panics,
// the test fails with the recovered value; otherwise the (config, err) pair
// is returned for assertion. The whole point of C5 is "no panics", so every
// test goes through this wrapper.
func callBuildInbound(t *testing.T, opt *conf.Options, info *panel.NodeInfo, tag string) (out struct {
	err error
}) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildInbound panicked: %v", r)
		}
	}()
	_, err := buildInbound(opt, info, tag)
	out.err = err
	return
}

func TestBuildInbound_NilNodeInfo(t *testing.T) {
	got := callBuildInbound(t, xrayOpts(), nil, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "nodeInfo") {
		t.Fatalf("expected nodeInfo error, got %v", got.err)
	}
}

func TestBuildInbound_NilCommon(t *testing.T) {
	got := callBuildInbound(t, xrayOpts(), &panel.NodeInfo{Type: "vless"}, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "Common") {
		t.Fatalf("expected Common nil error, got %v", got.err)
	}
}

func TestBuildInbound_NilOption(t *testing.T) {
	info := &panel.NodeInfo{Common: &panel.CommonNode{Protocol: "vless"}}
	got := callBuildInbound(t, nil, info, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "option") {
		t.Fatalf("expected option nil error, got %v", got.err)
	}
}

func TestBuildInbound_NilXrayOptions(t *testing.T) {
	opt := &conf.Options{ListenIP: "0.0.0.0"}
	info := &panel.NodeInfo{Common: &panel.CommonNode{Protocol: "vless"}}
	got := callBuildInbound(t, opt, info, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "XrayOptions") {
		t.Fatalf("expected XrayOptions nil error, got %v", got.err)
	}
}

func TestBuildInbound_VlessProtocolButVlessNil(t *testing.T) {
	info := &panel.NodeInfo{Common: &panel.CommonNode{Protocol: "vless"}}
	got := callBuildInbound(t, xrayOpts(), info, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "Common.Vless is nil") {
		t.Fatalf("expected Common.Vless nil error, got %v", got.err)
	}
}

func TestBuildInbound_VmessProtocolButVmessNil(t *testing.T) {
	info := &panel.NodeInfo{Common: &panel.CommonNode{Protocol: "vmess"}}
	got := callBuildInbound(t, xrayOpts(), info, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "Common.Vmess is nil") {
		t.Fatalf("expected Common.Vmess nil error, got %v", got.err)
	}
}

func TestBuildInbound_TrojanProtocolButTrojanNil(t *testing.T) {
	info := &panel.NodeInfo{Common: &panel.CommonNode{Protocol: "trojan"}}
	got := callBuildInbound(t, xrayOpts(), info, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "Common.Trojan is nil") {
		t.Fatalf("expected Common.Trojan nil error, got %v", got.err)
	}
}

func TestBuildInbound_ShadowsocksProtocolButShadowsocksNil(t *testing.T) {
	info := &panel.NodeInfo{Common: &panel.CommonNode{Protocol: "shadowsocks"}}
	got := callBuildInbound(t, xrayOpts(), info, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "Common.Shadowsocks is nil") {
		t.Fatalf("expected Common.Shadowsocks nil error, got %v", got.err)
	}
}

func TestBuildInbound_VlessTLS_MissingCertConfig(t *testing.T) {
	opt := xrayOpts()
	opt.CertConfig = nil // explicitly clear
	info := &panel.NodeInfo{
		Common: &panel.CommonNode{
			Protocol: "vless",
			Vless: &panel.VlessNode{
				Port:     8101,
				Network:  "tcp",
				Security: "tls",
				SecurityConfig: &panel.SecurityConfig{
					SNI: "vless.test",
				},
			},
		},
	}
	got := callBuildInbound(t, opt, info, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "CertConfig") {
		t.Fatalf("expected CertConfig error, got %v", got.err)
	}
}

func TestBuildInbound_VlessTLS_NilSecurityConfig_NoPanic(t *testing.T) {
	// security="tls" path doesn't dereference SecurityConfig directly, but
	// the test exists to lock in the no-panic guarantee for any future
	// edits that try to read SNI / cert fields without nil-checking.
	info := &panel.NodeInfo{
		Common: &panel.CommonNode{
			Protocol: "vless",
			Vless: &panel.VlessNode{
				Port:           8101,
				Network:        "tcp",
				Security:       "tls",
				SecurityConfig: nil,
			},
		},
	}
	got := callBuildInbound(t, xrayOpts(), info, "xray|test")
	// We accept either nil error (TLS off because CertMode=="none") or a
	// downstream error from xray-core's in.Build. The bar is "no panic".
	_ = got
}

func vlessReality(sc *panel.SecurityConfig) *panel.NodeInfo {
	return &panel.NodeInfo{
		Common: &panel.CommonNode{
			Protocol: "vless",
			Vless: &panel.VlessNode{
				Port:           8104,
				Network:        "tcp",
				Security:       "reality",
				SecurityConfig: sc,
			},
		},
	}
}

func TestBuildInbound_Reality_MissingSecurityConfig(t *testing.T) {
	got := callBuildInbound(t, xrayOpts(), vlessReality(nil), "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "missing security_config") {
		t.Fatalf("expected missing security_config error, got %v", got.err)
	}
}

func TestBuildInbound_Reality_MissingSNI(t *testing.T) {
	sc := &panel.SecurityConfig{
		RealityServerAddress: "1.2.3.4",
		RealityServerPort:    443,
		RealityPrivateKey:    "wEbNI8QwM1XLgX-ucy7Qwp6msGmGCfSMQClC-VRjV3w",
	}
	got := callBuildInbound(t, xrayOpts(), vlessReality(sc), "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "missing sni") {
		t.Fatalf("expected missing sni error, got %v", got.err)
	}
}

func TestBuildInbound_Reality_MissingPrivateKey(t *testing.T) {
	sc := &panel.SecurityConfig{
		SNI:                  "reality.test",
		RealityServerAddress: "1.2.3.4",
		RealityServerPort:    443,
	}
	got := callBuildInbound(t, xrayOpts(), vlessReality(sc), "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "missing reality_private_key") {
		t.Fatalf("expected missing reality_private_key error, got %v", got.err)
	}
}

func TestBuildInbound_Reality_BadPort(t *testing.T) {
	sc := &panel.SecurityConfig{
		SNI:                  "reality.test",
		RealityServerAddress: "1.2.3.4",
		RealityServerPort:    0,
		RealityPrivateKey:    "wEbNI8QwM1XLgX-ucy7Qwp6msGmGCfSMQClC-VRjV3w",
	}
	got := callBuildInbound(t, xrayOpts(), vlessReality(sc), "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "reality_server_port") {
		t.Fatalf("expected reality_server_port error, got %v", got.err)
	}
}

func TestBuildInbound_Reality_AddressFallsBackToSNI(t *testing.T) {
	// No RealityServerAddress, but SNI is present — our validator should
	// accept and use SNI as the dest.
	sc := &panel.SecurityConfig{
		SNI:               "reality.test",
		RealityServerPort: 443,
		RealityPrivateKey: "wEbNI8QwM1XLgX-ucy7Qwp6msGmGCfSMQClC-VRjV3w",
	}
	got := callBuildInbound(t, xrayOpts(), vlessReality(sc), "xray|test")
	// Acceptable: nil err (full success) or any downstream xray error.
	// Critical: not a "missing reality_server_addr" error.
	if got.err != nil && strings.Contains(got.err.Error(), "missing reality_server_addr") {
		t.Fatalf("address should fall back to SNI, but got: %v", got.err)
	}
}

func TestBuildInbound_Reality_CompleteConfig_NoValidationError(t *testing.T) {
	sc := &panel.SecurityConfig{
		SNI:                  "reality.test",
		RealityServerAddress: "www.cloudflare.com",
		RealityServerPort:    443,
		RealityPrivateKey:    "wEbNI8QwM1XLgX-ucy7Qwp6msGmGCfSMQClC-VRjV3w",
		RealityShortId:       "0123456789abcdef",
	}
	got := callBuildInbound(t, xrayOpts(), vlessReality(sc), "xray|test")
	// The reality validator should not produce any of the "invalid reality
	// config: missing X" errors. xray-core's own Build() may still reject
	// the config (e.g. private-key shape) — that's acceptable here.
	if got.err != nil && strings.Contains(got.err.Error(), "invalid reality config") {
		t.Fatalf("complete reality config rejected by validator: %v", got.err)
	}
}

func TestBuildInbound_Vless_NilTransportConfig_NoPanic(t *testing.T) {
	// Reproduces the exact P0 scenario reported by the operator: panel
	// returns vless with Network="tcp" but no transport_config sub-object.
	// Pre-C5 this caused a nil deref at inbound.go:85.
	info := &panel.NodeInfo{
		Common: &panel.CommonNode{
			Protocol: "vless",
			Vless: &panel.VlessNode{
				Port:            8101,
				Network:         "tcp",
				TransportConfig: nil, // <- the trigger
				Security:        "",
			},
		},
	}
	got := callBuildInbound(t, xrayOpts(), info, "xray|test")
	// tcp doesn't actually need TransportConfig; the call should succeed
	// (or at worst return a non-panic xray-core error).
	_ = got
}

func TestBuildInbound_UnsupportedProtocol(t *testing.T) {
	info := &panel.NodeInfo{Common: &panel.CommonNode{Protocol: "wireguard"}}
	got := callBuildInbound(t, xrayOpts(), info, "xray|test")
	if got.err == nil || !strings.Contains(got.err.Error(), "unsupported node type") {
		t.Fatalf("expected unsupported protocol error, got %v", got.err)
	}
}
