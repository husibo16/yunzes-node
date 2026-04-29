package node

import (
	"fmt"
	"sync"

	"github.com/husibo16/yunzes-node/api/panel"
)

// listenerSpec captures one (addr, port, transport) tuple a controller wants
// to bind. addr is already normalized: empty becomes "0.0.0.0".
type listenerSpec struct {
	addr      string
	port      uint16
	transport string // "tcp" | "udp"
}

func (s listenerSpec) String() string {
	return fmt.Sprintf("%s:%d/%s", s.addr, s.port, s.transport)
}

type listenerOwner struct {
	listenerSpec
	runtimeKey string
	logicalTag string
}

// portRegistry rejects three classes of duplicates at controller-Start time:
//
//  1. same runtimeKey registered twice
//  2. same logicalTag registered twice (even across different cores)
//  3. listener (addr,port,transport) overlap, including wildcard-aware cases:
//     - "0.0.0.0:port/tcp" vs "192.0.2.1:port/tcp"  -> conflict
//     - "::"+"0.0.0.0" same port/transport          -> conflict
//     - "443/tcp" vs "443/udp"                      -> ok (different transport)
//     - two specific IPv4 addresses on same port    -> ok
//
// Empty addresses are normalized to "0.0.0.0" before comparison so the
// caller can pass the raw Options.ListenIP straight in.
type portRegistry struct {
	mu          sync.Mutex
	listeners   []listenerOwner
	logicalTags map[string]string   // logicalTag -> owning runtimeKey
	runtimeKeys map[string]struct{} // dedupe
}

func newPortRegistry() *portRegistry {
	return &portRegistry{
		logicalTags: make(map[string]string),
		runtimeKeys: make(map[string]struct{}),
	}
}

// reserve atomically claims all listenerSpecs for (runtimeKey, logicalTag).
// On any conflict no state is mutated and an error describing the offender
// is returned.
func (r *portRegistry) reserve(runtimeKey, logicalTag string, specs []listenerSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.runtimeKeys[runtimeKey]; dup {
		return fmt.Errorf("duplicate runtime key %q", runtimeKey)
	}
	if owner, dup := r.logicalTags[logicalTag]; dup {
		return fmt.Errorf("duplicate logical tag %q (already registered by runtime key %q)", logicalTag, owner)
	}
	for _, spec := range specs {
		spec.addr = normalizeListenAddr(spec.addr)
		for _, e := range r.listeners {
			if listenerConflict(spec, e.listenerSpec) {
				return fmt.Errorf("listener %s conflicts with existing %s owned by runtime key %q (logical tag %q)",
					spec, e.listenerSpec, e.runtimeKey, e.logicalTag)
			}
		}
	}
	r.runtimeKeys[runtimeKey] = struct{}{}
	r.logicalTags[logicalTag] = runtimeKey
	for _, spec := range specs {
		spec.addr = normalizeListenAddr(spec.addr)
		r.listeners = append(r.listeners, listenerOwner{
			listenerSpec: spec,
			runtimeKey:   runtimeKey,
			logicalTag:   logicalTag,
		})
	}
	return nil
}

// release removes everything owned by runtimeKey. Safe to call when the key
// was never reserved (no-op).
func (r *portRegistry) release(runtimeKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.runtimeKeys[runtimeKey]; !ok {
		return
	}
	delete(r.runtimeKeys, runtimeKey)
	for k, v := range r.logicalTags {
		if v == runtimeKey {
			delete(r.logicalTags, k)
			break
		}
	}
	out := r.listeners[:0]
	for _, e := range r.listeners {
		if e.runtimeKey != runtimeKey {
			out = append(out, e)
		}
	}
	// Truncate with explicit nil so the underlying array can be GC'd if the
	// registry shrinks dramatically (hot-reload churn).
	for i := len(out); i < len(r.listeners); i++ {
		r.listeners[i] = listenerOwner{}
	}
	r.listeners = out
}

func normalizeListenAddr(addr string) string {
	if addr == "" {
		return "0.0.0.0"
	}
	return addr
}

// listenerConflict implements the conservative wildcard-aware rule:
//   - different transport or port: never conflict.
//   - same exact addr: conflict.
//   - "0.0.0.0" vs any specific IPv4: conflict (wildcard catches).
//   - "0.0.0.0" vs "::": conflict (dual-stack ambiguity).
//   - "::" vs specific IPv4: NOT a conflict (per spec, only :: ↔ 0.0.0.0).
//   - two distinct specific IPv4: NOT a conflict.
func listenerConflict(a, b listenerSpec) bool {
	if a.transport != b.transport || a.port != b.port {
		return false
	}
	if a.addr == b.addr {
		return true
	}
	wild4 := func(s string) bool { return s == "0.0.0.0" }
	wild6 := func(s string) bool { return s == "::" }
	specificV4 := func(s string) bool { return !wild4(s) && !wild6(s) }
	if (wild4(a.addr) && specificV4(b.addr)) || (specificV4(a.addr) && wild4(b.addr)) {
		return true
	}
	if (wild4(a.addr) && wild6(b.addr)) || (wild6(a.addr) && wild4(b.addr)) {
		return true
	}
	return false
}

// listenerSpecsFor returns the (addr, port, transport) tuples to reserve for
// a given node. shadowsocks always claims tcp+udp; hysteria/hysteria2/tuic
// claim udp; vless/vmess/trojan/anytls claim tcp. addr comes from
// options.ListenIP (normalized inside reserve).
func listenerSpecsFor(node *panel.NodeInfo, listenIP string) ([]listenerSpec, error) {
	port, err := protocolPort(node)
	if err != nil {
		return nil, err
	}
	transports := protocolTransports(node.Common.Protocol)
	if len(transports) == 0 {
		return nil, fmt.Errorf("no transport mapping for protocol %q", node.Common.Protocol)
	}
	specs := make([]listenerSpec, 0, len(transports))
	for _, t := range transports {
		specs = append(specs, listenerSpec{
			addr:      listenIP,
			port:      port,
			transport: t,
		})
	}
	return specs, nil
}

// protocolTransports returns the L4 transports a protocol binds at the
// listener level. shadowsocks defaults to tcp+udp; an explicit DisableUDP
// downgrade is a future C-stage concern.
func protocolTransports(proto string) []string {
	switch proto {
	case "shadowsocks":
		return []string{"tcp", "udp"}
	case "hysteria", "hysteria2", "tuic":
		return []string{"udp"}
	case "vless", "vmess", "trojan", "anytls":
		return []string{"tcp"}
	}
	return nil
}

func protocolPort(node *panel.NodeInfo) (uint16, error) {
	c := node.Common
	if c == nil {
		return 0, fmt.Errorf("node %q has no Common payload", node.Type)
	}
	switch c.Protocol {
	case "vless":
		if c.Vless == nil {
			return 0, fmt.Errorf("vless node %q missing Vless payload", node.Type)
		}
		return uint16(c.Vless.Port), nil
	case "vmess":
		if c.Vmess == nil {
			return 0, fmt.Errorf("vmess node %q missing Vmess payload", node.Type)
		}
		return uint16(c.Vmess.Port), nil
	case "trojan":
		if c.Trojan == nil {
			return 0, fmt.Errorf("trojan node %q missing Trojan payload", node.Type)
		}
		return uint16(c.Trojan.Port), nil
	case "shadowsocks":
		if c.Shadowsocks == nil {
			return 0, fmt.Errorf("shadowsocks node %q missing Shadowsocks payload", node.Type)
		}
		return uint16(c.Shadowsocks.Port), nil
	case "tuic":
		if c.Tuic == nil {
			return 0, fmt.Errorf("tuic node %q missing Tuic payload", node.Type)
		}
		return uint16(c.Tuic.Port), nil
	case "hysteria", "hysteria2":
		if c.Hysteria2 == nil {
			return 0, fmt.Errorf("hysteria2 node %q missing Hysteria2 payload", node.Type)
		}
		return uint16(c.Hysteria2.Port), nil
	case "anytls":
		if c.AnyTLS == nil {
			return 0, fmt.Errorf("anytls node %q missing AnyTLS payload", node.Type)
		}
		return uint16(c.AnyTLS.Port), nil
	}
	return 0, fmt.Errorf("unsupported protocol for port lookup: %s", c.Protocol)
}
