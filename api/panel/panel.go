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
	"github.com/perfect-panel/ppanel-node/conf"
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
	Mode                string `json:"mode"`
	Extra               string `json:"extra"`
	Cipher              string `json:"cipher"`
	ServerKey           string `json:"server_key"`
	Flow                string `json:"flow"`
	ObfsPassword        string `json:"obfs_password"`
	PaddingScheme       string `json:"padding_scheme"`
	UpMbps              int    `json:"up_mbps"`
	DownMbps            int    `json:"down_mbps"`
	EnableProxyProtocol bool   `json:"enable_proxy_protocol"`
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
		SetHeader("secret_key", apiConfig.SecretKey).
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
