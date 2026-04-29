package node

import (
	"strings"
	"testing"
)

func TestListenerConflict_Matrix(t *testing.T) {
	cases := []struct {
		name string
		a, b listenerSpec
		want bool
	}{
		{"diff transport same port", spec("0.0.0.0", 443, "tcp"), spec("0.0.0.0", 443, "udp"), false},
		{"diff port same transport", spec("0.0.0.0", 80, "tcp"), spec("0.0.0.0", 443, "tcp"), false},
		{"same addr same port same transport", spec("1.2.3.4", 80, "tcp"), spec("1.2.3.4", 80, "tcp"), true},
		{"wildcard4 vs specific v4", spec("0.0.0.0", 80, "tcp"), spec("1.2.3.4", 80, "tcp"), true},
		{"specific v4 vs wildcard4", spec("1.2.3.4", 80, "tcp"), spec("0.0.0.0", 80, "tcp"), true},
		{"wildcard6 vs wildcard4", spec("::", 443, "tcp"), spec("0.0.0.0", 443, "tcp"), true},
		{"wildcard4 vs wildcard6", spec("0.0.0.0", 443, "tcp"), spec("::", 443, "tcp"), true},
		{"wildcard6 vs specific v4", spec("::", 443, "tcp"), spec("1.2.3.4", 443, "tcp"), false},
		{"two distinct specific v4", spec("1.2.3.4", 80, "tcp"), spec("5.6.7.8", 80, "tcp"), false},
		{"443/tcp coexists with 443/udp", spec("0.0.0.0", 443, "tcp"), spec("0.0.0.0", 443, "udp"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := listenerConflict(tc.a, tc.b); got != tc.want {
				t.Errorf("listenerConflict(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestNormalizeListenAddr(t *testing.T) {
	if got := normalizeListenAddr(""); got != "0.0.0.0" {
		t.Errorf("empty string should normalize to 0.0.0.0, got %q", got)
	}
	if got := normalizeListenAddr("::"); got != "::" {
		t.Errorf(":: should pass through, got %q", got)
	}
	if got := normalizeListenAddr("1.2.3.4"); got != "1.2.3.4" {
		t.Errorf("specific addr should pass through, got %q", got)
	}
}

func TestPortRegistry_Reserve_HappyPath(t *testing.T) {
	r := newPortRegistry()
	if err := r.reserve("xray|vless1", "vless1", []listenerSpec{spec("0.0.0.0", 443, "tcp")}); err != nil {
		t.Fatalf("first reserve should succeed: %v", err)
	}
	if err := r.reserve("xray|vless2", "vless2", []listenerSpec{spec("0.0.0.0", 8080, "tcp")}); err != nil {
		t.Fatalf("non-conflicting reserve should succeed: %v", err)
	}
}

func TestPortRegistry_Reserve_DuplicateRuntimeKey(t *testing.T) {
	r := newPortRegistry()
	_ = r.reserve("xray|t1", "t1", []listenerSpec{spec("0.0.0.0", 1, "tcp")})
	err := r.reserve("xray|t1", "t1b", []listenerSpec{spec("0.0.0.0", 2, "tcp")})
	if err == nil || !strings.Contains(err.Error(), "duplicate runtime key") {
		t.Fatalf("expected duplicate runtime key error, got %v", err)
	}
}

func TestPortRegistry_Reserve_DuplicateLogicalTag(t *testing.T) {
	r := newPortRegistry()
	_ = r.reserve("xray|t1", "shared", []listenerSpec{spec("0.0.0.0", 1, "tcp")})
	err := r.reserve("sing|t2", "shared", []listenerSpec{spec("0.0.0.0", 2, "udp")})
	if err == nil || !strings.Contains(err.Error(), "duplicate logical tag") {
		t.Fatalf("expected duplicate logical tag error, got %v", err)
	}
}

func TestPortRegistry_Reserve_PortConflict(t *testing.T) {
	r := newPortRegistry()
	_ = r.reserve("xray|a", "a", []listenerSpec{spec("0.0.0.0", 443, "tcp")})
	err := r.reserve("xray|b", "b", []listenerSpec{spec("1.2.3.4", 443, "tcp")})
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("expected listener conflict error, got %v", err)
	}
}

func TestPortRegistry_Reserve_TcpUdpCoexist(t *testing.T) {
	r := newPortRegistry()
	if err := r.reserve("xray|a", "a", []listenerSpec{spec("0.0.0.0", 443, "tcp")}); err != nil {
		t.Fatalf("tcp reserve: %v", err)
	}
	if err := r.reserve("sing|b", "b", []listenerSpec{spec("0.0.0.0", 443, "udp")}); err != nil {
		t.Fatalf("udp on same port should coexist: %v", err)
	}
}

func TestPortRegistry_Reserve_AtomicOnFailure(t *testing.T) {
	// If multi-spec reserve hits a conflict on the second spec, neither
	// listener nor the runtimeKey/logicalTag entries should be committed.
	r := newPortRegistry()
	_ = r.reserve("xray|owner", "owner", []listenerSpec{spec("0.0.0.0", 8388, "udp")})
	err := r.reserve("xray|new", "new", []listenerSpec{
		spec("0.0.0.0", 8388, "tcp"), // ok
		spec("0.0.0.0", 8388, "udp"), // conflicts with owner
	})
	if err == nil {
		t.Fatalf("expected conflict on second spec")
	}
	// The dedupe sets should NOT carry the failed reservation.
	if _, ok := r.runtimeKeys["xray|new"]; ok {
		t.Errorf("runtimeKey should not be committed on partial-failure")
	}
	if _, ok := r.logicalTags["new"]; ok {
		t.Errorf("logicalTag should not be committed on partial-failure")
	}
	for _, l := range r.listeners {
		if l.runtimeKey == "xray|new" {
			t.Errorf("no listener should be committed for failed reservation")
		}
	}
}

func TestPortRegistry_Release(t *testing.T) {
	r := newPortRegistry()
	_ = r.reserve("xray|a", "a", []listenerSpec{spec("0.0.0.0", 443, "tcp")})
	r.release("xray|a")
	if err := r.reserve("xray|a", "a", []listenerSpec{spec("0.0.0.0", 443, "tcp")}); err != nil {
		t.Fatalf("after release the same key should be reusable: %v", err)
	}
	// release of an unknown key should be a no-op (no panic).
	r.release("does-not-exist")
}

func spec(addr string, port uint16, transport string) listenerSpec {
	return listenerSpec{addr: addr, port: port, transport: transport}
}
