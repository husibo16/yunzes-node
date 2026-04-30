package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
	"github.com/husibo16/yunzes-node/conf"
)

type Client struct {
	Client           *resty.Client
	APIHost          string
	Token            string
	NodeType         string
	NodeId           int
	nodeEtag         string
	userEtag         string
	responseBodyHash string
	UserList         *UserListBody
	AliveMap         *AliveMap
}

func New(c *conf.ApiConfig) (*Client, error) {
	client := resty.New()
	client.SetRetryCount(3)
	if c.Timeout > 0 {
		client.SetTimeout(time.Duration(c.Timeout) * time.Second)
	} else {
		client.SetTimeout(5 * time.Second)
	}
	client.OnError(func(req *resty.Request, err error) {
		var v *resty.ResponseError
		if errors.As(err, &v) {
			// v.Response contains the last response from the server
			// v.Err contains the original error
			logrus.Error(v.Err)
		}
	})
	client.SetBaseURL(c.APIHost)
	// Check node type
	c.NodeType = strings.ToLower(c.NodeType)
	switch c.NodeType {
	case "v2ray":
		c.NodeType = "vmess"
	case
		"vmess",
		"trojan",
		"shadowsocks",
		"tuic",
		"hysteria2",
		"anytls",
		"vless":
	default:
		return nil, fmt.Errorf("unsupported Node type: %s", c.NodeType)
	}
	// set params
	client.SetQueryParams(map[string]string{
		"protocol":   c.NodeType,
		"server_id":  strconv.Itoa(c.NodeID),
		"secret_key": c.Key,
	})
	return &Client{
		Client:   client,
		Token:    c.Key,
		APIHost:  c.APIHost,
		NodeType: c.NodeType,
		NodeId:   c.NodeID,
		UserList: &UserListBody{},
		AliveMap: &AliveMap{},
	}, nil
}

type ServerProtocolConfigResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Basic     *BasicConfig     `json:"basic"`
		Protocols []ProtocolConfig `json:"protocols"`
	} `json:"data"`
}

type ProtocolConfig struct {
	Type                string `json:"type"`
	Port                int    `json:"port"`
	Security            string `json:"security"`
	SNI                 string `json:"sni"`
	RealityServerAddr   string `json:"reality_server_addr"`
	RealityServerPort   int    `json:"reality_server_port"`
	RealityPrivateKey   string `json:"reality_private_key"`
	RealityShortID      string `json:"reality_short_id"`
	RealityMldsa65seed  string `json:"reality_mldsa65seed"`
	Transport           string `json:"transport"`
	Host                string `json:"host"`
	Path                string `json:"path"`
	ServiceName         string `json:"service_name"`
	// XhttpMode / XhttpExtra are the xhttp transport's "mode" and "extra"
	// settings. The wire JSON keys are xhttp_mode / xhttp_extra (matching
	// the panel's flat Protocol struct); the local Go fields stay
	// PascalCase. They feed into TransportConfig.Mode / TransportConfig.Extra
	// in buildServerController. Older nodes used to parse "mode" / "extra"
	// as the JSON keys, which silently dropped the value because the panel
	// always sent xhttp_mode / xhttp_extra.
	XhttpMode           string `json:"xhttp_mode"`
	XhttpExtra          string `json:"xhttp_extra"`
	Cipher              string `json:"cipher"`
	ServerKey           string `json:"server_key"`
	Flow                string `json:"flow"`
	ObfsPassword        string `json:"obfs_password"`
	PaddingScheme       string `json:"padding_scheme"`
	UpMbps              int    `json:"up_mbps"`
	DownMbps            int    `json:"down_mbps"`
	EnableProxyProtocol bool   `json:"enable_proxy_protocol"`
	// Flat cert_* fields the panel server ships in its types.Protocol DTO.
	// resolveCertConfig consumes these when CertConfig (the nested object
	// below) is nil — without this, admins who set CertMode=dns/file/self
	// in the panel had their setting silently downgraded to "http" because
	// the legacy fallback hardcoded that mode.
	CertMode        string `json:"cert_mode,omitempty"`
	CertDNSProvider string `json:"cert_dns_provider,omitempty"`
	CertDNSEnv      string `json:"cert_dns_env,omitempty"` // "KEY=VALUE\n..." textarea form
	// CertConfig, if non-nil, lets the panel server override the
	// hardcoded ACME HTTP-01 default that the panel-driven controller
	// previously baked in for every Security="tls" protocol. Old servers
	// that don't yet send this field leave the pointer nil and the node
	// falls back to the flat CertMode / CertDNSProvider / CertDNSEnv above
	// (or, if those are also empty, to the legacy HTTP-01 default with
	// p.SNI and /etc/yunzes-node/certs/<type><id>.{crt,key} paths). New
	// servers can set CertMode to dns/file/self/none and supply
	// Provider / Email / DNSEnv / RenewBeforeDays / explicit cert paths.
	// Resolution lives in node.resolveCertConfig.
	CertConfig *CertProtocolConfig `json:"cert_config,omitempty"`
}

// CertProtocolConfig is the wire-side cert override the panel server may
// attach to a ProtocolConfig. Each field is optional; empty values fall
// back to the legacy defaults that the node used to hardcode (see
// node.resolveCertConfig). Snake-case JSON tags match the rest of the
// /v2/server payload; the in-process conf.CertConfig stays PascalCase
// because it is also written to local config files.
type CertProtocolConfig struct {
	CertMode        string            `json:"cert_mode"`
	CertDomain      string            `json:"cert_domain"`
	CertFile        string            `json:"cert_file"`
	KeyFile         string            `json:"key_file"`
	Provider        string            `json:"provider"`
	Email           string            `json:"email"`
	DNSEnv          map[string]string `json:"dns_env"`
	RenewBeforeDays int               `json:"renew_before_days"`
}

func GetServerNodeConfigs(apiConfig *conf.ServerApiConfig) ([]ProtocolConfig, *BasicConfig, error) {
	client := resty.New()
	client.SetRetryCount(3)
	if apiConfig.Timeout > 0 {
		client.SetTimeout(time.Duration(apiConfig.Timeout) * time.Second)
	} else {
		client.SetTimeout(5 * time.Second)
	}
	client.OnError(func(req *resty.Request, err error) {
		var v *resty.ResponseError
		if errors.As(err, &v) {
			// v.Response contains the last response from the server
			// v.Err contains the original error
			logrus.Error(v.Err)
		}
	})
	client.SetBaseURL(apiConfig.ApiHost)
	path := fmt.Sprintf("/v2/server/%d", apiConfig.ServerId)
	r, err := client.
		R().
		SetQueryParam("secret_key", apiConfig.SecretKey).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, nil, fmt.Errorf("request %s failed: %v", client.BaseURL+path, err.Error())
	}
	if r.StatusCode() >= 400 {
		body := r.Body()
		return nil, nil, fmt.Errorf("request %s failed: %s", client.BaseURL+path, string(body))
	}
	if r != nil {
		defer func() {
			if r.RawBody() != nil {
				r.RawBody().Close()
			}
		}()
	} else {
		return nil, nil, fmt.Errorf("received nil response")
	}
	resp := &ServerProtocolConfigResponse{}
	err = json.Unmarshal(r.Body(), resp)
	if err != nil {
		return nil, nil, fmt.Errorf("decode node params error: %s", err)
	}
	if len(resp.Data.Protocols) == 0 {
		return nil, nil, fmt.Errorf("no protocol config found")
	}
	return resp.Data.Protocols, resp.Data.Basic, nil
}
