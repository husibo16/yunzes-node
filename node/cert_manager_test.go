package node

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/husibo16/yunzes-node/conf"
	log "github.com/sirupsen/logrus"
)

func writeTestCert(t *testing.T, certPath, keyPath, domain string, notAfter time.Time) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	tmpl := &x509.Certificate{
		Version:               3,
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: domain},
		DNSNames:              []string{domain},
		BasicConstraintsValid: true,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func newTestCertConfig(t *testing.T, mode, domain string) *conf.CertConfig {
	t.Helper()
	dir := t.TempDir()
	return &conf.CertConfig{
		CertMode:   mode,
		CertDomain: domain,
		CertFile:   filepath.Join(dir, "cert.pem"),
		KeyFile:    filepath.Join(dir, "key.pem"),
	}
}

func mustEnsure(t *testing.T, cfg *conf.CertConfig, wantAction string) {
	t.Helper()
	resetCertClaimsForTest()
	le := log.NewEntry(log.New())
	le.Logger.SetOutput(os.Stderr)
	le.Logger.SetLevel(log.DebugLevel)
	got, err := EnsureCertificate(cfg, le)
	if err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	if got != wantAction {
		t.Fatalf("action = %q, want %q", got, wantAction)
	}
}

func TestEnsureCertificate_NoneMode_NoOp(t *testing.T) {
	mustEnsure(t, &conf.CertConfig{CertMode: "none"}, "")
	mustEnsure(t, &conf.CertConfig{CertMode: ""}, "")
}

func TestEnsureCertificate_FileMode_HappyPath(t *testing.T) {
	cfg := newTestCertConfig(t, "file", "vpn.example.com")
	writeTestCert(t, cfg.CertFile, cfg.KeyFile, "vpn.example.com", time.Now().Add(90*24*time.Hour))
	mustEnsure(t, cfg, CertActionReuse)
}

func TestEnsureCertificate_FileMode_Missing(t *testing.T) {
	cfg := newTestCertConfig(t, "file", "vpn.example.com")
	resetCertClaimsForTest()
	_, err := EnsureCertificate(cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestEnsureCertificate_FileMode_DomainMismatch(t *testing.T) {
	cfg := newTestCertConfig(t, "file", "vpn.example.com")
	writeTestCert(t, cfg.CertFile, cfg.KeyFile, "different.example.com", time.Now().Add(90*24*time.Hour))
	resetCertClaimsForTest()
	_, err := EnsureCertificate(cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "does not cover domain") {
		t.Fatalf("expected domain-mismatch error, got %v", err)
	}
}

func TestEnsureCertificate_SelfMode_IssueWhenMissing(t *testing.T) {
	cfg := newTestCertConfig(t, "self", "self.test")
	mustEnsure(t, cfg, CertActionIssue)
	if _, err := os.Stat(cfg.CertFile); err != nil {
		t.Fatalf("cert file should exist after issue: %v", err)
	}
	if _, err := os.Stat(cfg.KeyFile); err != nil {
		t.Fatalf("key file should exist after issue: %v", err)
	}
	cert, err := parseDiskCert(cfg.CertFile)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !certCoversDomain(cert, "self.test") {
		t.Fatalf("self-signed cert should cover its domain")
	}
}

func TestEnsureCertificate_SelfMode_ReuseWhenHealthy(t *testing.T) {
	cfg := newTestCertConfig(t, "self", "self.test")
	mustEnsure(t, cfg, CertActionIssue)
	mustEnsure(t, cfg, CertActionReuse)
}

func TestEnsureCertificate_SelfMode_ReissueWhenCorrupt(t *testing.T) {
	cfg := newTestCertConfig(t, "self", "self.test")
	if err := os.WriteFile(cfg.CertFile, []byte("not a pem"), 0644); err != nil {
		t.Fatalf("seed corrupt cert: %v", err)
	}
	if err := os.WriteFile(cfg.KeyFile, []byte("not a pem"), 0600); err != nil {
		t.Fatalf("seed corrupt key: %v", err)
	}
	mustEnsure(t, cfg, CertActionReissue)
	if _, err := parseDiskCert(cfg.CertFile); err != nil {
		t.Fatalf("after reissue cert should parse: %v", err)
	}
}

func TestEnsureCertificate_DomainMismatchBlocksHTTP(t *testing.T) {
	cfg := newTestCertConfig(t, "http", "vpn.example.com")
	cfg.Email = "test@example.com"
	cfg.Provider = "manual"
	writeTestCert(t, cfg.CertFile, cfg.KeyFile, "different.example.com", time.Now().Add(90*24*time.Hour))
	resetCertClaimsForTest()
	_, err := EnsureCertificate(cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "does not cover domain") {
		t.Fatalf("expected domain-mismatch block, got %v", err)
	}
}

func TestEnsureCertificate_RenewBeforeDays_Default(t *testing.T) {
	if got := effectiveRenewBeforeDays(&conf.CertConfig{}); got != 30 {
		t.Errorf("default RenewBeforeDays = %d, want 30", got)
	}
	if got := effectiveRenewBeforeDays(&conf.CertConfig{RenewBeforeDays: 7}); got != 7 {
		t.Errorf("RenewBeforeDays = %d, want 7", got)
	}
	if got := renewBefore(&conf.CertConfig{}); got != 30*24*time.Hour {
		t.Errorf("renewBefore default = %v, want 30 days", got)
	}
	if got := renewBefore(&conf.CertConfig{RenewBeforeDays: 14}); got != 14*24*time.Hour {
		t.Errorf("renewBefore 14 = %v, want 14 days", got)
	}
}

func TestClaimCertFiles_DifferentDomainSameFile(t *testing.T) {
	resetCertClaimsForTest()
	if err := claimCertFiles("/tmp/yz_a.pem", "/tmp/yz_a.key", "a.com"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	err := claimCertFiles("/tmp/yz_a.pem", "/tmp/yz_a.key", "b.com")
	if err == nil || !strings.Contains(err.Error(), "already claimed by domain") {
		t.Fatalf("expected fatal block on different domain, got %v", err)
	}
}

func TestClaimCertFiles_SameDomainReclaim(t *testing.T) {
	resetCertClaimsForTest()
	if err := claimCertFiles("/tmp/yz_b.pem", "/tmp/yz_b.key", "a.com"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := claimCertFiles("/tmp/yz_b.pem", "/tmp/yz_b.key", "a.com"); err != nil {
		t.Fatalf("re-claim with same domain should succeed: %v", err)
	}
}

func TestClaimCertFiles_SameDomainDifferentKey(t *testing.T) {
	resetCertClaimsForTest()
	if err := claimCertFiles("/tmp/yz_c.pem", "/tmp/yz_c.key", "a.com"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	err := claimCertFiles("/tmp/yz_c.pem", "/tmp/yz_other.key", "a.com")
	if err == nil || !strings.Contains(err.Error(), "paired with KeyFile") {
		t.Fatalf("expected key-file mismatch block, got %v", err)
	}
}

func TestWriteFileAtomic_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	if err := writeFileAtomic(path, []byte("hello"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
}

func TestWriteFileAtomic_PreservesOldOnSuccess(t *testing.T) {
	// Two consecutive writes — the second should fully replace the first
	// without leaving any tmp files behind.
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	if err := writeFileAtomic(path, []byte("first"), 0644); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := writeFileAtomic(path, []byte("second"), 0644); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "second" {
		t.Fatalf("content = %q, want %q", got, "second")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly one file in %s, got %v", dir, names)
	}
}

func TestCertTaskKey_Stability(t *testing.T) {
	a := &conf.CertConfig{
		CertMode:   "http",
		CertDomain: "a.com",
		CertFile:   "/tmp/cert.pem",
		KeyFile:    "/tmp/key.pem",
		Provider:   "cloudflare",
		Email:      "x@example.com",
	}
	b := *a
	if certTaskKey(a) != certTaskKey(&b) {
		t.Fatalf("identical configs should share a key")
	}
	b.Email = "y@example.com"
	if certTaskKey(a) == certTaskKey(&b) {
		t.Fatalf("differing Email must change key")
	}
	c := *a
	c.CertFile = "/tmp/other.pem"
	if certTaskKey(a) == certTaskKey(&c) {
		t.Fatalf("differing CertFile must change key")
	}
}

func TestCertCoversDomain(t *testing.T) {
	cert := &x509.Certificate{
		Subject:  pkix.Name{CommonName: "vpn.example.com"},
		DNSNames: []string{"vpn.example.com", "www.example.com"},
	}
	if !certCoversDomain(cert, "vpn.example.com") {
		t.Errorf("DNSNames match should pass")
	}
	if !certCoversDomain(cert, "www.example.com") {
		t.Errorf("alt DNSNames match should pass")
	}
	if certCoversDomain(cert, "other.example.com") {
		t.Errorf("non-listed domain should fail")
	}
	if !certCoversDomain(cert, "") {
		t.Errorf("empty domain is treated as 'don't check'")
	}
	cnOnly := &x509.Certificate{
		Subject: pkix.Name{CommonName: "fallback.test"},
	}
	if !certCoversDomain(cnOnly, "fallback.test") {
		t.Errorf("CommonName fallback should pass when SAN is empty")
	}
}

func TestEnsureCertificate_ACMEDuplicateRequiresValidation(t *testing.T) {
	// Empty CertDomain on an ACME mode is a hard error.
	cfg := newTestCertConfig(t, "http", "")
	resetCertClaimsForTest()
	_, err := EnsureCertificate(cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "CertDomain") {
		t.Fatalf("expected CertDomain-required error, got %v", err)
	}
}

func TestEnsureCertificate_UnknownMode(t *testing.T) {
	cfg := &conf.CertConfig{CertMode: "bogus"}
	resetCertClaimsForTest()
	_, err := EnsureCertificate(cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported CertMode") {
		t.Fatalf("expected unsupported CertMode error, got %v", err)
	}
}
