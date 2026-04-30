package sing

import (
	"strings"
	"testing"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/conf"
)

// TestGetInboundOptions_UnknownProtocolReturnsError is the C17
// regression. Pre-fix the default branch ran `fmt.Println("Unknown
// protocol:", ...)` and let the function continue with zero-value
// port/security, eventually nil-panicking on c.SingOptions or
// returning an Inbound with empty Options that sing-box's in.Create
// rejected with no protocol-name context.
//
// Post-fix the default branch returns an explicit error that names
// the offending protocol, so AddNode propagates it up to
// controller.Start / reload where the structured logger surfaces it
// with the full runtime-key context.
func TestGetInboundOptions_UnknownProtocolReturnsError(t *testing.T) {
	info := &panel.NodeInfo{
		Type: "wireguard", // unsupported by the sing core in this fork
		Common: &panel.CommonNode{
			Protocol: "wireguard",
		},
	}
	opts := &conf.Options{
		ListenIP: "0.0.0.0",
		// SingOptions is intentionally nil — pre-fix the function
		// fell through to `c.SingOptions.TCPFastOpen` and nil-panicked
		// here. The post-fix early return must trigger before any
		// dereference of c.SingOptions.
	}

	in, err := getInboundOptions("test-tag", info, opts)
	if err == nil {
		t.Fatalf("expected error for unknown protocol, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported sing protocol") {
		t.Errorf("error text = %q, want it to contain \"unsupported sing protocol\"", err.Error())
	}
	if !strings.Contains(err.Error(), "wireguard") {
		t.Errorf("error text = %q, want it to name the offending protocol \"wireguard\"", err.Error())
	}
	if in.Tag != "" || in.Type != "" {
		t.Errorf("on error path Inbound must be zero-value, got %+v", in)
	}
}

// TestGetInboundOptions_EmptyProtocolReturnsError covers the edge of
// an entirely missing protocol string — also routes through the
// default branch and must surface as a typed error.
func TestGetInboundOptions_EmptyProtocolReturnsError(t *testing.T) {
	info := &panel.NodeInfo{
		Common: &panel.CommonNode{Protocol: ""},
	}
	opts := &conf.Options{ListenIP: "0.0.0.0"}

	_, err := getInboundOptions("test-tag", info, opts)
	if err == nil {
		t.Fatalf("expected error for empty protocol, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported sing protocol") {
		t.Errorf("error text = %q, want \"unsupported sing protocol\"", err.Error())
	}
}

// TestGetInboundOptions_InvalidListenIPReturnsError locks in the
// pre-existing error path so a future refactor of getInboundOptions
// (e.g. moving the protocol switch above the listen-IP parse) doesn't
// silently drop this validation.
func TestGetInboundOptions_InvalidListenIPReturnsError(t *testing.T) {
	info := &panel.NodeInfo{
		Common: &panel.CommonNode{Protocol: "shadowsocks"},
	}
	opts := &conf.Options{ListenIP: "not-an-ip"}

	_, err := getInboundOptions("test-tag", info, opts)
	if err == nil {
		t.Fatalf("expected error for invalid listen ip, got nil")
	}
	if !strings.Contains(err.Error(), "listen ip") {
		t.Errorf("error text = %q, want it to mention \"listen ip\"", err.Error())
	}
}
