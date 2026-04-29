package format

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidateShadowsocksCipher_Whitelist(t *testing.T) {
	for _, c := range ShadowsocksAllowedCiphers {
		var key string
		if want := shadowsocks2022KeyLen(c); want > 0 {
			key = base64.StdEncoding.EncodeToString(make([]byte, want))
		}
		if err := ValidateShadowsocksCipher(c, key); err != nil {
			t.Errorf("whitelist cipher %q rejected: %v", c, err)
		}
	}
}

func TestValidateShadowsocksCipher_Rejects(t *testing.T) {
	cases := []struct {
		name, cipher, key, wantSubstr string
	}{
		{"empty", "", "", "empty"},
		{"none", "none", "", "not allowed"},
		{"plain", "plain", "", "not allowed"},
		{"unknown", "rc4", "", "not allowed"},
		{"2022-missing-key", "2022-blake3-aes-128-gcm", "", "non-empty ServerKey"},
		{"2022-bad-base64", "2022-blake3-aes-128-gcm", "!!!!!!!!", "base64"},
		{"2022-wrong-len", "2022-blake3-aes-256-gcm", base64.StdEncoding.EncodeToString(make([]byte, 16)), "32 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateShadowsocksCipher(tc.cipher, tc.key)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}
