package node

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/husibo16/yunzes-node/conf"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// DefaultRenewBeforeDays is the renewal threshold used when a CertConfig
// leaves RenewBeforeDays at its zero value.
const DefaultRenewBeforeDays = 30

// Cert lifecycle actions reported by EnsureCertificate.
const (
	CertActionIssue   = "issue"   // first-time obtain (no file or only-not-exist)
	CertActionRenew   = "renew"   // existing file, near or past expiry
	CertActionReuse   = "reuse"   // existing file, healthy and far from expiry
	CertActionReissue = "reissue" // existing file is corrupt/unparseable
	CertActionError   = "error"
)

// certClaim records which CertDomain owns a particular CertFile (and its
// paired KeyFile). The first call for a given CertFile wins; later calls with
// a different CertDomain are rejected to keep two controllers from
// overwriting each other's certificate.
type certClaim struct {
	domain  string
	keyFile string
}

var (
	certClaimMu sync.Mutex
	certClaims  = map[string]certClaim{}

	// certIssuanceMu serializes ACME http-01/dns-01 acquires across the whole
	// process. Two cert configs cannot race for port 80 (HTTP-01) and DNS-01
	// providers cannot race their global env-var setup.
	certIssuanceMu sync.Mutex

	// certGroup deduplicates concurrent EnsureCertificate calls with an
	// identical certTaskKey — e.g. an initial Start and the periodic
	// renewCertTask both firing in the same window for one node.
	certGroup singleflight.Group
)

// EnsureCertificate inspects the on-disk cert at cfg.CertFile, decides what
// to do per the lifecycle table below, and returns the action it took.
//
//	CertMode  | disk state                     | action
//	----------+--------------------------------+----------
//	none/""   | (any)                          | no-op
//	file      | both files present + valid     | reuse
//	file      | missing or invalid             | error
//	self      | missing / corrupt / mismatched | issue/reissue (self-signed)
//	self      | valid                          | reuse
//	http/dns  | missing                        | issue   (ACME)
//	http/dns  | corrupt                        | reissue (ACME)
//	http/dns  | domain mismatch                | error (block)
//	http/dns  | within RenewBeforeDays         | renew   (ACME)
//	http/dns  | expired                        | renew   (ACME)
//	http/dns  | healthy                        | reuse
//
// Disk is the source of truth: every call stat+parses the cert. There is no
// in-memory cache that influences the issue/renew decision.
func EnsureCertificate(cfg *conf.CertConfig, le *log.Entry) (string, error) {
	if cfg == nil {
		return "", errors.New("nil CertConfig")
	}
	if le == nil {
		le = log.NewEntry(log.StandardLogger())
	}

	switch cfg.CertMode {
	case "", "none":
		return "", nil
	case "file":
		return ensureFileMode(cfg, le)
	case "self", "http", "dns":
		// fall through
	default:
		return CertActionError, fmt.Errorf("unsupported CertMode %q", cfg.CertMode)
	}

	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return CertActionError, errors.New("CertFile and KeyFile must be set")
	}
	if cfg.CertDomain == "" {
		return CertActionError, errors.New("CertDomain must be set")
	}
	if err := claimCertFiles(cfg.CertFile, cfg.KeyFile, cfg.CertDomain); err != nil {
		return CertActionError, err
	}

	key := certTaskKey(cfg)
	v, err, _ := certGroup.Do(key, func() (any, error) {
		return ensureLifecycle(cfg, le)
	})
	if err != nil {
		return CertActionError, err
	}
	return v.(string), nil
}

func ensureFileMode(cfg *conf.CertConfig, le *log.Entry) (string, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return CertActionError, errors.New("file mode requires CertFile and KeyFile")
	}
	if !fileExists(cfg.CertFile) || !fileExists(cfg.KeyFile) {
		return CertActionError, fmt.Errorf("file mode: cert/key not found at %s / %s", cfg.CertFile, cfg.KeyFile)
	}
	cert, err := parseDiskCert(cfg.CertFile)
	if err != nil {
		return CertActionError, fmt.Errorf("file mode: cert unparseable: %s", err)
	}
	if !certCoversDomain(cert, cfg.CertDomain) {
		return CertActionError, fmt.Errorf("file mode: cert at %s does not cover domain %q (CN=%q SAN=%v)",
			cfg.CertFile, cfg.CertDomain, cert.Subject.CommonName, cert.DNSNames)
	}
	logCertEvent(le, CertActionReuse, cfg, time.Until(cert.NotAfter))
	return CertActionReuse, nil
}

func ensureLifecycle(cfg *conf.CertConfig, le *log.Entry) (string, error) {
	cert, parseErr := parseDiskCert(cfg.CertFile)

	if parseErr != nil && os.IsNotExist(parseErr) {
		return acquire(cfg, le, CertActionIssue)
	}
	if parseErr != nil {
		le.WithFields(certBaseFields(cfg, CertActionReissue, 0)).
			WithField("err", parseErr).
			Warn("cert file unparseable, reissuing")
		return acquire(cfg, le, CertActionReissue)
	}

	// Domain mismatch is a hard fail per spec — refuse to overwrite a cert
	// for a different domain even if the operator pointed two configs at the
	// same file by accident.
	if !certCoversDomain(cert, cfg.CertDomain) {
		return CertActionError, fmt.Errorf("cert at %s does not cover domain %q (CN=%q SAN=%v)",
			cfg.CertFile, cfg.CertDomain, cert.Subject.CommonName, cert.DNSNames)
	}

	remaining := time.Until(cert.NotAfter)
	threshold := renewBefore(cfg)

	if remaining <= 0 {
		le.WithFields(certBaseFields(cfg, CertActionRenew, remaining)).
			Info("cert expired, renewing")
		return acquire(cfg, le, CertActionRenew)
	}
	if remaining < threshold {
		le.WithFields(certBaseFields(cfg, CertActionRenew, remaining)).
			Info("cert within renewal window, renewing")
		return acquire(cfg, le, CertActionRenew)
	}

	logCertEvent(le, CertActionReuse, cfg, remaining)
	return CertActionReuse, nil
}

// acquire performs the actual issue/renew work for self/http/dns modes. ACME
// modes are serialized through certIssuanceMu so two concurrent acquires
// can't fight over port 80 or DNS-01 env-vars.
func acquire(cfg *conf.CertConfig, le *log.Entry, action string) (string, error) {
	if cfg.CertMode == "self" {
		if err := writeSelfSignedCert(cfg.CertDomain, cfg.CertFile, cfg.KeyFile); err != nil {
			return CertActionError, err
		}
		var remaining time.Duration
		if cert, err := parseDiskCert(cfg.CertFile); err == nil {
			remaining = time.Until(cert.NotAfter)
		}
		logCertEvent(le, action, cfg, remaining)
		return action, nil
	}

	certIssuanceMu.Lock()
	defer certIssuanceMu.Unlock()

	legoClient, err := NewLego(cfg)
	if err != nil {
		return CertActionError, fmt.Errorf("create lego: %s", err)
	}

	if action == CertActionRenew {
		if err := legoClient.Renew(); err != nil {
			// Renew can fail when the on-disk cert is no longer recognized
			// by the ACME server (e.g. account rotated). Fall back to a
			// fresh Obtain so the controller still gets a working cert.
			le.WithFields(certBaseFields(cfg, action, 0)).
				WithField("err", err).
				Warn("renew failed, falling back to fresh issue")
			if err := legoClient.Obtain(); err != nil {
				return CertActionError, fmt.Errorf("obtain after renew failure: %s", err)
			}
		}
	} else {
		if err := legoClient.Obtain(); err != nil {
			return CertActionError, fmt.Errorf("obtain: %s", err)
		}
	}

	var remaining time.Duration
	if cert, err := parseDiskCert(cfg.CertFile); err == nil {
		remaining = time.Until(cert.NotAfter)
	}
	logCertEvent(le, action, cfg, remaining)
	return action, nil
}

// certTaskKey is the singleflight dedupe key. Ordering and presence of every
// field that influences ACME behavior (mode, domain, paths, provider, email)
// matters — two configs that differ in any of these must NOT share a flight.
// NUL is used as the join because none of the inputs can legitimately
// contain it.
func certTaskKey(cfg *conf.CertConfig) string {
	return strings.Join([]string{
		cfg.CertMode,
		cfg.CertDomain,
		cfg.CertFile,
		cfg.KeyFile,
		cfg.Provider,
		cfg.Email,
	}, "\x00")
}

func renewBefore(cfg *conf.CertConfig) time.Duration {
	d := cfg.RenewBeforeDays
	if d <= 0 {
		d = DefaultRenewBeforeDays
	}
	return time.Duration(d) * 24 * time.Hour
}

// claimCertFiles locks (CertFile, CertDomain) into a process-wide table.
// Calling again with the same domain is a no-op (controller restart). A
// different domain pointing at the same file is fatal.
func claimCertFiles(certFile, keyFile, domain string) error {
	certClaimMu.Lock()
	defer certClaimMu.Unlock()
	if existing, ok := certClaims[certFile]; ok {
		if existing.domain != domain {
			return fmt.Errorf("CertFile %q is already claimed by domain %q; refusing to overwrite for %q",
				certFile, existing.domain, domain)
		}
		if existing.keyFile != keyFile {
			return fmt.Errorf("CertFile %q is paired with KeyFile %q; refusing to repair with %q",
				certFile, existing.keyFile, keyFile)
		}
	}
	if existing, ok := certClaims[keyFile]; ok && existing.domain != domain {
		return fmt.Errorf("KeyFile %q is already claimed by domain %q; refusing to overwrite for %q",
			keyFile, existing.domain, domain)
	}
	claim := certClaim{domain: domain, keyFile: keyFile}
	certClaims[certFile] = claim
	certClaims[keyFile] = claim
	return nil
}

// resetCertClaimsForTest is used only by cert_manager_test.go to reset
// process-wide state between subtests.
func resetCertClaimsForTest() {
	certClaimMu.Lock()
	certClaims = map[string]certClaim{}
	certClaimMu.Unlock()
}

func parseDiskCert(p string) (*x509.Certificate, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", p)
	}
	return x509.ParseCertificate(block.Bytes)
}

func certCoversDomain(cert *x509.Certificate, domain string) bool {
	if domain == "" {
		return true
	}
	for _, n := range cert.DNSNames {
		if n == domain {
			return true
		}
	}
	return cert.Subject.CommonName == domain
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writeFileAtomic writes data to p via a sibling temp file plus fsync plus
// rename. A crash between any of these steps leaves the previous file (if
// any) intact at the destination. The temp file is removed on any failure.
func writeFileAtomic(p string, data []byte, perm os.FileMode) (retErr error) {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %s", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".cert-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %s", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %s", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync temp: %s", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %s", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp: %s", err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		return fmt.Errorf("rename: %s", err)
	}
	return nil
}

func certBaseFields(cfg *conf.CertConfig, action string, remaining time.Duration) log.Fields {
	f := log.Fields{
		"cert_action":       action,
		"domain":            cfg.CertDomain,
		"cert_file":         cfg.CertFile,
		"key_file":          cfg.KeyFile,
		"renew_before_days": effectiveRenewBeforeDays(cfg),
	}
	if remaining > 0 {
		f["remaining"] = remaining.Round(time.Hour).String()
	}
	return f
}

func logCertEvent(le *log.Entry, action string, cfg *conf.CertConfig, remaining time.Duration) {
	le.WithFields(certBaseFields(cfg, action, remaining)).Info("certificate")
}

func effectiveRenewBeforeDays(cfg *conf.CertConfig) int {
	if cfg.RenewBeforeDays > 0 {
		return cfg.RenewBeforeDays
	}
	return DefaultRenewBeforeDays
}
