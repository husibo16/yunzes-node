package node

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/http01"
	"github.com/go-acme/lego/v4/providers/dns"
	"github.com/go-acme/lego/v4/registration"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/lego"
	"github.com/husibo16/yunzes-node/common/file"
	"github.com/husibo16/yunzes-node/conf"
)

type Lego struct {
	client *lego.Client
	config *conf.CertConfig
}

// NewLego builds a configured ACME client. The "when to renew" decision lives
// in EnsureCertificate; this type only wraps Obtain / Renew.
//
// HTTP-01 binds to host port 80 and DNS-01 mutates process-global env-vars.
// Callers must hold node.certIssuanceMu across the entire NewLego ->
// Obtain/Renew lifecycle so two configs cannot fight for those resources.
func NewLego(config *conf.CertConfig) (*Lego, error) {
	user, err := NewLegoUser(path.Join(path.Dir(config.CertFile),
		"user",
		fmt.Sprintf("user-%s.json", config.Email)),
		config.Email)
	if err != nil {
		return nil, fmt.Errorf("create user error: %s", err)
	}
	c := lego.NewConfig(user)
	c.Certificate.KeyType = certcrypto.RSA2048
	client, err := lego.NewClient(c)
	if err != nil {
		return nil, err
	}
	l := Lego{
		client: client,
		config: config,
	}
	if err := l.SetProvider(); err != nil {
		return nil, fmt.Errorf("set provider error: %s", err)
	}
	return &l, nil
}

func checkPath(p string) error {
	if !file.IsExist(path.Dir(p)) {
		err := os.MkdirAll(path.Dir(p), 0755)
		if err != nil {
			return fmt.Errorf("create dir error: %s", err)
		}
	}
	return nil
}

func (l *Lego) SetProvider() error {
	switch l.config.CertMode {
	case "http":
		err := l.client.Challenge.SetHTTP01Provider(http01.NewProviderServer("", "80"))
		if err != nil {
			return err
		}
	case "dns":
		// Caller (EnsureCertificate) owns certIssuanceMu so concurrent DNS-01
		// configs cannot trample each other's env vars between SetEnv and the
		// ACME call. Cross-call leakage is acceptable: the next holder will
		// overwrite to its own values before issuing.
		for k, v := range l.config.DNSEnv {
			os.Setenv(k, v)
		}
		p, err := dns.NewDNSChallengeProviderByName(l.config.Provider)
		if err != nil {
			return fmt.Errorf("create dns challenge provider error: %s", err)
		}
		err = l.client.Challenge.SetDNS01Provider(p)
		if err != nil {
			return fmt.Errorf("set dns provider error: %s", err)
		}
	}
	return nil
}

// Obtain acquires a fresh certificate via ACME and writes it atomically.
// The caller is responsible for deciding when an obtain is required.
func (l *Lego) Obtain() error {
	request := certificate.ObtainRequest{
		Domains: []string{l.config.CertDomain},
		Bundle:  true,
	}
	res, err := l.client.Certificate.Obtain(request)
	if err != nil {
		return fmt.Errorf("obtain certificate error: %s", err)
	}
	return l.writeCert(res)
}

// Renew renews the certificate currently on disk. Unlike the previous
// implementation it no longer second-guesses the "should I renew now?" gate
// — that lives in EnsureCertificate and uses the configurable
// RenewBeforeDays threshold.
func (l *Lego) Renew() error {
	pemBytes, err := os.ReadFile(l.config.CertFile)
	if err != nil {
		return fmt.Errorf("read cert file error: %s", err)
	}
	res, err := l.client.Certificate.Renew(certificate.Resource{
		Domain:      l.config.CertDomain,
		Certificate: pemBytes,
	}, true, false, "")
	if err != nil {
		return err
	}
	return l.writeCert(res)
}

func (l *Lego) parseParams(p string) string {
	r := strings.NewReplacer("{domain}", l.config.CertDomain,
		"{email}", l.config.Email)
	return r.Replace(p)
}

// writeCert persists cert + key atomically. The temp-file + fsync + rename
// pattern guarantees that a crash mid-write leaves the previous good cert
// intact and never produces a partially-written file at the destination.
func (l *Lego) writeCert(res *certificate.Resource) error {
	certPath := l.parseParams(l.config.CertFile)
	keyPath := l.parseParams(l.config.KeyFile)
	if err := writeFileAtomic(certPath, res.Certificate, 0644); err != nil {
		return fmt.Errorf("write cert file: %s", err)
	}
	// Private keys are 0600 — anything more permissive is a security smell.
	if err := writeFileAtomic(keyPath, res.PrivateKey, 0600); err != nil {
		return fmt.Errorf("write key file: %s", err)
	}
	return nil
}

type User struct {
	Email        string                 `json:"Email"`
	Registration *registration.Resource `json:"Registration"`
	key          crypto.PrivateKey
	KeyEncoded   string `json:"Key"`
}

func (u *User) GetEmail() string {
	return u.Email
}

func (u *User) GetRegistration() *registration.Resource {
	return u.Registration
}

func (u *User) GetPrivateKey() crypto.PrivateKey {
	return u.key
}

func NewLegoUser(path string, email string) (*User, error) {
	var user User
	if file.IsExist(path) {
		err := user.Load(path)
		if err != nil {
			return nil, err
		}
		if user.Email != email {
			user.Registration = nil
			user.Email = email
			err := registerUser(&user, path)
			if err != nil {
				return nil, err
			}
		}
	} else {
		user.Email = email
		err := registerUser(&user, path)
		if err != nil {
			return nil, err
		}
	}
	return &user, nil
}

func registerUser(user *User, path string) error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key error: %s", err)
	}
	user.key = privateKey
	c := lego.NewConfig(user)
	client, err := lego.NewClient(c)
	if err != nil {
		return fmt.Errorf("create lego client error: %s", err)
	}
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return err
	}
	user.Registration = reg
	err = user.Save(path)
	if err != nil {
		return fmt.Errorf("save user error: %s", err)
	}
	return nil
}

func EncodePrivate(privKey *ecdsa.PrivateKey) (string, error) {
	encoded, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return "", err
	}
	pemEncoded := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded})
	return string(pemEncoded), nil
}

func (u *User) Save(p string) error {
	if err := checkPath(p); err != nil {
		return fmt.Errorf("check path error: %s", err)
	}
	u.KeyEncoded, _ = EncodePrivate(u.key.(*ecdsa.PrivateKey))
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("marshal json error: %s", err)
	}
	if err := writeFileAtomic(p, data, 0600); err != nil {
		return err
	}
	u.KeyEncoded = ""
	return nil
}

func (u *User) DecodePrivate(pemEncodedPriv string) (*ecdsa.PrivateKey, error) {
	blockPriv, _ := pem.Decode([]byte(pemEncodedPriv))
	x509EncodedPriv := blockPriv.Bytes
	privateKey, err := x509.ParseECPrivateKey(x509EncodedPriv)
	return privateKey, err
}

func (u *User) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("open file error: %s", err)
	}
	err = json.Unmarshal(data, u)
	if err != nil {
		return fmt.Errorf("unmarshal json error: %s", err)
	}
	u.key, err = u.DecodePrivate(u.KeyEncoded)
	if err != nil {
		return fmt.Errorf("decode private key error: %s", err)
	}
	return nil
}
