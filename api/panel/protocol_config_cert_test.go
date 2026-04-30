package panel

import (
	"encoding/json"
	"testing"
)

// TestProtocolConfig_CertConfigJSONRoundtrip locks in the wire shape
// the panel server is expected to emit. snake_case keys, all fields
// optional, the whole CertConfig object itself optional via omitempty.
func TestProtocolConfig_CertConfigJSONRoundtrip(t *testing.T) {
	in := ProtocolConfig{
		Type:     "vless",
		Port:     8443,
		Security: "tls",
		SNI:      "vpn.example.com",
		CertConfig: &CertProtocolConfig{
			CertMode:        "dns",
			CertDomain:      "*.example.com",
			Provider:        "cloudflare",
			Email:           "ops@example.com",
			DNSEnv:          map[string]string{"CF_DNS_API_TOKEN": "tok"},
			RenewBeforeDays: 21,
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out ProtocolConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.CertConfig == nil {
		t.Fatalf("CertConfig lost across roundtrip: %s", string(raw))
	}
	if out.CertConfig.CertMode != "dns" || out.CertConfig.Provider != "cloudflare" {
		t.Fatalf("CertConfig fields lost: %+v", out.CertConfig)
	}
	if out.CertConfig.DNSEnv["CF_DNS_API_TOKEN"] != "tok" {
		t.Fatalf("DNSEnv map lost: %+v", out.CertConfig.DNSEnv)
	}

	// Wire shape sanity: payload must use snake_case keys, not the
	// PascalCase that conf.CertConfig uses on disk.
	wantSubstrings := []string{
		`"cert_config":`,
		`"cert_mode":"dns"`,
		`"provider":"cloudflare"`,
		`"renew_before_days":21`,
		`"dns_env":{"CF_DNS_API_TOKEN":"tok"}`,
	}
	for _, sub := range wantSubstrings {
		if !contains(string(raw), sub) {
			t.Errorf("wire payload missing %q. got: %s", sub, string(raw))
		}
	}
}

// TestProtocolConfig_OldServerNoCertConfigUnmarshals confirms the
// backward-compat shape: a payload from an old server that does not
// know about cert_config still unmarshals cleanly with CertConfig=nil.
func TestProtocolConfig_OldServerNoCertConfigUnmarshals(t *testing.T) {
	const oldPayload = `{
		"type": "vless",
		"port": 8443,
		"security": "tls",
		"sni": "vpn.example.com",
		"reality_server_addr": "",
		"transport": "tcp"
	}`
	var p ProtocolConfig
	if err := json.Unmarshal([]byte(oldPayload), &p); err != nil {
		t.Fatalf("unmarshal old-shape payload: %v", err)
	}
	if p.CertConfig != nil {
		t.Fatalf("old-shape payload must leave CertConfig nil, got %+v", p.CertConfig)
	}
	if p.SNI != "vpn.example.com" {
		t.Fatalf("rest of the payload must still decode; got SNI=%q", p.SNI)
	}
}

// TestProtocolConfig_EmptyCertConfigUnmarshals — explicit empty object
// {} must unmarshal as a non-nil CertProtocolConfig with zero fields,
// so resolveCertConfig's empty-mode-fallback-to-http path is what runs.
func TestProtocolConfig_EmptyCertConfigUnmarshals(t *testing.T) {
	const payload = `{
		"type": "vless",
		"security": "tls",
		"sni": "x",
		"cert_config": {}
	}`
	var p ProtocolConfig
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.CertConfig == nil {
		t.Fatalf("explicit cert_config:{} must produce a non-nil pointer")
	}
	if p.CertConfig.CertMode != "" {
		t.Fatalf("zero-value CertMode expected, got %q", p.CertConfig.CertMode)
	}
}

// contains is a tiny strings.Contains-without-importing helper so this
// file stays import-light.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
