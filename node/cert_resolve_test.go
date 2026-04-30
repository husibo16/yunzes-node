package node

import (
	"reflect"
	"testing"

	"github.com/husibo16/yunzes-node/api/panel"
)

// TestResolveCertConfig_NilCertConfigKeepsLegacyBehavior is the
// backward-compat regression. An old server returning ProtocolConfig
// without a CertConfig field must produce the exact same conf.CertConfig
// that the previous hardcoded block built (ACME HTTP-01 + p.SNI domain
// + default /etc paths).
func TestResolveCertConfig_NilCertConfigKeepsLegacyBehavior(t *testing.T) {
	p := panel.ProtocolConfig{
		Type:     "vless",
		Security: "tls",
		SNI:      "vpn.example.com",
		// CertConfig is nil — the legacy shape.
	}
	got := resolveCertConfig(p, "vless", 42)

	if got.CertMode != "http" {
		t.Errorf("CertMode = %q, want \"http\" (legacy default)", got.CertMode)
	}
	if got.CertDomain != "vpn.example.com" {
		t.Errorf("CertDomain = %q, want %q (from p.SNI)", got.CertDomain, "vpn.example.com")
	}
	if got.CertFile != "/etc/yunzes-node/certs/vless42.crt" {
		t.Errorf("CertFile = %q, want default /etc path", got.CertFile)
	}
	if got.KeyFile != "/etc/yunzes-node/certs/vless42.key" {
		t.Errorf("KeyFile = %q, want default /etc path", got.KeyFile)
	}
	if got.RejectUnknownSni != false {
		t.Errorf("RejectUnknownSni = %v, want false (legacy hardcoded)", got.RejectUnknownSni)
	}
	if got.Email != "" || got.Provider != "" || got.DNSEnv != nil || got.RenewBeforeDays != 0 {
		t.Errorf("legacy path leaked non-zero fields: %+v", got)
	}
}

// TestResolveCertConfig_FullDNSOverride exercises the most demanded
// production case: server delivering ACME DNS-01 settings with
// provider, email, and DNSEnv map.
func TestResolveCertConfig_FullDNSOverride(t *testing.T) {
	p := panel.ProtocolConfig{
		Type:     "trojan",
		Security: "tls",
		SNI:      "wildcard.example.com",
		CertConfig: &panel.CertProtocolConfig{
			CertMode:        "dns",
			Provider:        "cloudflare",
			Email:           "ops@example.com",
			DNSEnv:          map[string]string{"CF_DNS_API_TOKEN": "token-xyz"},
			RenewBeforeDays: 14,
			// CertDomain / CertFile / KeyFile intentionally empty — must
			// fall back to defaults.
		},
	}
	got := resolveCertConfig(p, "trojan", 7)

	if got.CertMode != "dns" {
		t.Errorf("CertMode = %q, want %q", got.CertMode, "dns")
	}
	if got.CertDomain != "wildcard.example.com" {
		t.Errorf("CertDomain = %q, want fallback to p.SNI", got.CertDomain)
	}
	if got.CertFile != "/etc/yunzes-node/certs/trojan7.crt" {
		t.Errorf("CertFile = %q, want default /etc path", got.CertFile)
	}
	if got.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want %q", got.Provider, "cloudflare")
	}
	if got.Email != "ops@example.com" {
		t.Errorf("Email = %q, want %q", got.Email, "ops@example.com")
	}
	if !reflect.DeepEqual(got.DNSEnv, map[string]string{"CF_DNS_API_TOKEN": "token-xyz"}) {
		t.Errorf("DNSEnv mismatch: %+v", got.DNSEnv)
	}
	if got.RenewBeforeDays != 14 {
		t.Errorf("RenewBeforeDays = %d, want 14", got.RenewBeforeDays)
	}
}

// TestResolveCertConfig_FileModeWithCustomPaths exercises the file-mode
// path: operator pre-issued cert + key, server tells node where to find
// them (instead of the /etc default).
func TestResolveCertConfig_FileModeWithCustomPaths(t *testing.T) {
	p := panel.ProtocolConfig{
		Type:     "vless",
		Security: "tls",
		SNI:      "vpn.example.com",
		CertConfig: &panel.CertProtocolConfig{
			CertMode: "file",
			CertFile: "/var/secrets/vpn.crt",
			KeyFile:  "/var/secrets/vpn.key",
		},
	}
	got := resolveCertConfig(p, "vless", 1)

	if got.CertMode != "file" {
		t.Errorf("CertMode = %q, want %q", got.CertMode, "file")
	}
	if got.CertFile != "/var/secrets/vpn.crt" {
		t.Errorf("CertFile = %q, want operator path", got.CertFile)
	}
	if got.KeyFile != "/var/secrets/vpn.key" {
		t.Errorf("KeyFile = %q, want operator path", got.KeyFile)
	}
	if got.CertDomain != "vpn.example.com" {
		t.Errorf("CertDomain = %q, want fallback to p.SNI", got.CertDomain)
	}
}

// TestResolveCertConfig_SelfMode covers self-signed for testing /
// internal-only nodes; CertDomain still defaults from p.SNI.
func TestResolveCertConfig_SelfMode(t *testing.T) {
	p := panel.ProtocolConfig{
		Type:     "anytls",
		Security: "tls",
		SNI:      "internal.lab",
		CertConfig: &panel.CertProtocolConfig{
			CertMode: "self",
		},
	}
	got := resolveCertConfig(p, "anytls", 99)

	if got.CertMode != "self" {
		t.Errorf("CertMode = %q, want %q", got.CertMode, "self")
	}
	if got.CertDomain != "internal.lab" {
		t.Errorf("CertDomain = %q, want fallback to p.SNI", got.CertDomain)
	}
}

// TestResolveCertConfig_EmptyCertModeFallsBackToHttp documents the
// "server sent partial cert_config but didn't pick a mode" case: still
// degrade to the legacy ACME HTTP-01 default rather than fail
// downstream EnsureCertificate.
func TestResolveCertConfig_EmptyCertModeFallsBackToHttp(t *testing.T) {
	p := panel.ProtocolConfig{
		Type:     "vmess",
		Security: "tls",
		SNI:      "vpn.example.com",
		CertConfig: &panel.CertProtocolConfig{
			Email: "ops@example.com", // partial — only email given
		},
	}
	got := resolveCertConfig(p, "vmess", 5)

	if got.CertMode != "http" {
		t.Errorf("CertMode = %q, want fallback to \"http\"", got.CertMode)
	}
	if got.Email != "ops@example.com" {
		t.Errorf("Email passthrough lost: got %q", got.Email)
	}
}

// TestResolveCertConfig_ServerCertDomainOverridesSNI exercises the
// branching SNI vs. cert-domain split: a server may want to use a
// different SAN as the ACME identifier than the SNI shown to clients
// (e.g. wildcard cert covering multiple SNIs).
func TestResolveCertConfig_ServerCertDomainOverridesSNI(t *testing.T) {
	p := panel.ProtocolConfig{
		Type:     "vless",
		Security: "tls",
		SNI:      "client-facing.example.com",
		CertConfig: &panel.CertProtocolConfig{
			CertMode:   "dns",
			CertDomain: "*.example.com",
			Provider:   "cloudflare",
			Email:      "ops@example.com",
		},
	}
	got := resolveCertConfig(p, "vless", 3)

	if got.CertDomain != "*.example.com" {
		t.Errorf("CertDomain = %q, want explicit override over p.SNI", got.CertDomain)
	}
}

// TestResolveCertConfig_DNSEnvIsNotShared verifies we don't accidentally
// alias the DNSEnv map between the server payload and the resolved
// config. (We currently passthrough the same map reference; this test
// documents that contract.) If a future change starts deep-copying we
// can flip the assertion.
func TestResolveCertConfig_DNSEnvPassthroughDoesNotPanic(t *testing.T) {
	env := map[string]string{"K": "v"}
	p := panel.ProtocolConfig{
		Type:     "vless",
		Security: "tls",
		SNI:      "x",
		CertConfig: &panel.CertProtocolConfig{
			CertMode: "dns",
			DNSEnv:   env,
		},
	}
	got := resolveCertConfig(p, "vless", 1)
	if got.DNSEnv["K"] != "v" {
		t.Errorf("DNSEnv map not propagated")
	}
}

// TestResolveCertConfig_FlatCertFieldsPopulateDNSMode covers the wire
// format the panel server actually emits today: nested CertConfig is
// nil, but flat CertMode / CertDNSProvider / CertDNSEnv are populated
// from the admin form. Before this codepath existed, admins setting
// CertMode=dns saw their setting silently downgraded to "http" because
// resolveCertConfig hardcoded that mode whenever CertConfig was nil.
func TestResolveCertConfig_FlatCertFieldsPopulateDNSMode(t *testing.T) {
	p := panel.ProtocolConfig{
		Type:            "trojan",
		Security:        "tls",
		SNI:             "vpn.example.com",
		CertMode:        "dns",
		CertDNSProvider: "cloudflare",
		CertDNSEnv:      "CF_DNS_API_TOKEN=tok-abc\nCF_ACCOUNT_ID=acc-xyz",
	}
	got := resolveCertConfig(p, "trojan", 7)

	if got.CertMode != "dns" {
		t.Errorf("CertMode = %q, want \"dns\"", got.CertMode)
	}
	if got.CertDomain != "vpn.example.com" {
		t.Errorf("CertDomain = %q, want %q", got.CertDomain, "vpn.example.com")
	}
	if got.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want \"cloudflare\"", got.Provider)
	}
	wantEnv := map[string]string{
		"CF_DNS_API_TOKEN": "tok-abc",
		"CF_ACCOUNT_ID":    "acc-xyz",
	}
	if !reflect.DeepEqual(got.DNSEnv, wantEnv) {
		t.Errorf("DNSEnv = %v, want %v", got.DNSEnv, wantEnv)
	}
}

func TestParseFlatDNSEnv(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \n  \t  \n", nil},
		{"single", "K=V", map[string]string{"K": "V"}},
		{"multiple lines", "A=1\nB=2\nC=3", map[string]string{"A": "1", "B": "2", "C": "3"}},
		{"blank lines skipped", "A=1\n\nB=2\n\n", map[string]string{"A": "1", "B": "2"}},
		{"surrounding whitespace trimmed", "  A = 1 \n B=2\n", map[string]string{"A": "1", "B": "2"}},
		{"value with equals preserved", "TOKEN=abc=def=ghi", map[string]string{"TOKEN": "abc=def=ghi"}},
		{"line without equals skipped", "GOOD=1\njunk-no-equals\nALSO=2", map[string]string{"GOOD": "1", "ALSO": "2"}},
		{"empty key skipped", "=val\nGOOD=ok", map[string]string{"GOOD": "ok"}},
		{"only invalid lines returns nil", "no-equals\nanother", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseFlatDNSEnv(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseFlatDNSEnv(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
