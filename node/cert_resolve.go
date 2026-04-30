package node

import (
	"fmt"
	"strings"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/conf"
)

// parseFlatDNSEnv turns the panel admin's "KEY=VALUE\nKEY=VALUE" textarea
// string (carried on the wire as p.CertDNSEnv) into the map[string]string
// shape that lego.go consumes. Blank lines and lines without "=" are
// skipped silently. Whitespace around keys/values is trimmed; values are
// preserved verbatim otherwise so secrets containing "=" round-trip.
func parseFlatDNSEnv(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			continue
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// defaultPanelCertFile / defaultPanelCertKey return the on-disk paths the
// panel-driven controller pins TLS certs to when the server doesn't
// supply explicit paths. Format matches the legacy hardcoded shape
// previously baked into buildServerController; existing operator
// installs already have files at these paths so changing the layout
// would break in-place upgrades.
func defaultPanelCertFile(nodeType string, nodeId int) string {
	return fmt.Sprintf("/etc/yunzes-node/certs/%s%d.crt", nodeType, nodeId)
}

func defaultPanelCertKey(nodeType string, nodeId int) string {
	return fmt.Sprintf("/etc/yunzes-node/certs/%s%d.key", nodeType, nodeId)
}

// resolveCertConfig builds the conf.CertConfig that the panel-driven
// controller hands to EnsureCertificate / inbound TLS, taking the
// optional server-supplied CertConfig override into account.
//
// Backward compatibility contract:
//
//   - If p.CertConfig is nil (old server, or server choosing not to
//     override), the result is bit-for-bit equivalent to the legacy
//     hardcoded behavior: ACME HTTP-01, CertDomain=p.SNI, default
//     CertFile/KeyFile paths.
//
//   - If p.CertConfig is non-nil, every field falls back to the legacy
//     default when empty:
//
//     CertMode   "" -> "http"          (preserve previous default)
//     CertDomain "" -> p.SNI           (panel SNI is the canonical
//     identifier)
//     CertFile   "" -> default path    (operator may keep /etc default)
//     KeyFile    "" -> default path
//     Provider          : passthrough  (only used for dns mode)
//     Email             : passthrough  (only used for ACME modes)
//     DNSEnv            : passthrough  (only used for dns mode)
//     RenewBeforeDays   : passthrough  (0 means "use 30-day default"
//     per node.EnsureCertificate)
//
//   - Servers can now set CertMode to "dns" / "file" / "self" / "none"
//     and the rest of the fields will be honored. RejectUnknownSni is
//     intentionally NOT exposed via the wire — it stays false (legacy
//     hardcoded value); operators wanting strict SNI can flip it via
//     local config.
//
// This is called only when p.Security == "tls"; the "reality" / ""
// / "none" branches in buildServerController short-circuit to
// CertMode=none without consulting this function.
func resolveCertConfig(p panel.ProtocolConfig, nodeType string, nodeId int) *conf.CertConfig {
	defCertFile := defaultPanelCertFile(nodeType, nodeId)
	defKeyFile := defaultPanelCertKey(nodeType, nodeId)

	if p.CertConfig == nil {
		// No nested cert_config object on the wire. Today the panel server
		// does NOT emit one — it ships flat cert_mode / cert_dns_provider /
		// cert_dns_env fields instead — so try those before falling back
		// to the legacy HTTP-01 hardcode. Without this, admins setting
		// CertMode=dns in the panel had their setting silently downgraded
		// to "http".
		mode := p.CertMode
		if mode == "" {
			mode = "http"
		}
		return &conf.CertConfig{
			CertMode:         mode,
			RejectUnknownSni: false,
			CertDomain:       p.SNI,
			CertFile:         defCertFile,
			KeyFile:          defKeyFile,
			Provider:         p.CertDNSProvider,
			DNSEnv:           parseFlatDNSEnv(p.CertDNSEnv),
		}
	}

	cc := p.CertConfig
	mode := cc.CertMode
	if mode == "" {
		mode = "http"
	}
	domain := cc.CertDomain
	if domain == "" {
		domain = p.SNI
	}
	certFile := cc.CertFile
	if certFile == "" {
		certFile = defCertFile
	}
	keyFile := cc.KeyFile
	if keyFile == "" {
		keyFile = defKeyFile
	}

	return &conf.CertConfig{
		CertMode:         mode,
		RejectUnknownSni: false,
		CertDomain:       domain,
		CertFile:         certFile,
		KeyFile:          keyFile,
		Provider:         cc.Provider,
		Email:            cc.Email,
		DNSEnv:           cc.DNSEnv,
		RenewBeforeDays:  cc.RenewBeforeDays,
	}
}
