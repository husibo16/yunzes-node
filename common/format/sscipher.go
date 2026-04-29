package format

import (
	"encoding/base64"
	"fmt"
)

// ShadowsocksAllowedCiphers is the canonical whitelist of accepted shadowsocks
// methods. Anything outside this list (including "none", "plain", or empty)
// is rejected by ValidateShadowsocksCipher.
var ShadowsocksAllowedCiphers = []string{
	"aes-128-gcm",
	"aes-256-gcm",
	"chacha20-ietf-poly1305",
	"2022-blake3-aes-128-gcm",
	"2022-blake3-aes-256-gcm",
	"2022-blake3-chacha20-poly1305",
}

// shadowsocks2022KeyLen returns the required raw key length (in bytes) for a
// 2022-blake3-* cipher, or 0 if the cipher is not a 2022 variant.
func shadowsocks2022KeyLen(cipher string) int {
	switch cipher {
	case "2022-blake3-aes-128-gcm":
		return 16
	case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		return 32
	}
	return 0
}

// ValidateShadowsocksCipher enforces the cipher whitelist. For 2022-blake3-*
// ciphers it also verifies that ServerKey is non-empty, valid base64, and
// decodes to the cipher's required key length.
func ValidateShadowsocksCipher(cipher, serverKey string) error {
	if cipher == "" {
		return fmt.Errorf("shadowsocks cipher is empty; allowed: %v", ShadowsocksAllowedCiphers)
	}
	allowed := false
	for _, c := range ShadowsocksAllowedCiphers {
		if c == cipher {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("shadowsocks cipher %q is not allowed; allowed: %v", cipher, ShadowsocksAllowedCiphers)
	}
	if want := shadowsocks2022KeyLen(cipher); want > 0 {
		if serverKey == "" {
			return fmt.Errorf("shadowsocks cipher %q requires a non-empty ServerKey", cipher)
		}
		raw, err := base64.StdEncoding.DecodeString(serverKey)
		if err != nil {
			return fmt.Errorf("shadowsocks ServerKey is not valid base64: %s", err)
		}
		if len(raw) != want {
			return fmt.Errorf("shadowsocks cipher %q requires base64 ServerKey decoding to %d bytes, got %d", cipher, want, len(raw))
		}
	}
	return nil
}
