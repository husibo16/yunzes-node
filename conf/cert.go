package conf

type CertConfig struct {
	CertMode         string            `json:"CertMode"` // none, file, http, dns, self
	RejectUnknownSni bool              `json:"RejectUnknownSni"`
	CertDomain       string            `json:"CertDomain"`
	CertFile         string            `json:"CertFile"`
	KeyFile          string            `json:"KeyFile"`
	Provider         string            `json:"Provider"` // alidns, cloudflare, gandi, godaddy....
	Email            string            `json:"Email"`
	DNSEnv           map[string]string `json:"DNSEnv"`
	// RenewBeforeDays is how many days ahead of NotAfter the controller will
	// trigger a renewal. Zero or negative means use DefaultRenewBeforeDays
	// (30) in node.EnsureCertificate.
	RenewBeforeDays int `json:"RenewBeforeDays"`
}

func NewCertConfig() *CertConfig {
	return &CertConfig{
		CertMode: "none",
	}
}
