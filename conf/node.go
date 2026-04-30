package conf

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/husibo16/yunzes-node/common/json5"
)

type NodeConfig struct {
	ApiConfig ApiConfig `json:"-"`
	Options   Options   `json:"-"`
}

type rawNodeConfig struct {
	Include string          `json:"Include"`
	ApiRaw  json.RawMessage `json:"ApiConfig"`
	OptRaw  json.RawMessage `json:"Options"`
}

type ApiConfig struct {
	APIHost  string `json:"ApiHost"`
	NodeID   int    `json:"NodeID"`
	Key      string `json:"ApiKey"`
	NodeType string `json:"NodeType"`
	Timeout  int    `json:"Timeout"`
}

func (n *NodeConfig) UnmarshalJSON(data []byte) (err error) {
	rn := rawNodeConfig{}
	err = json.Unmarshal(data, &rn)
	if err != nil {
		return err
	}
	if len(rn.Include) != 0 {
		// Previously this ran:
		//
		//   file, _ := strings.CutPrefix(rn.Include, ":")
		//   switch file {
		//   case "http", "https":
		//       rsp, err := http.Get(file)   // ← Get("http") / Get("https"), not the URL
		//       ...
		//
		// Two bugs: CutPrefix(s, ":") returns s unchanged when s does
		// not start with ":" (it never does for a real URL), so `file`
		// was always the full Include string. The switch then never
		// hit "http"/"https" and we silently fell through to
		// os.Open(rn.Include), which fails for URLs. Even if the switch
		// had matched, the http.Get target was the SCHEME literal
		// instead of the URL. Result: remote Include never worked.
		//
		// Fix: scheme-detect via prefix on rn.Include itself, and use
		// rn.Include verbatim for both http.Get and os.Open. Also
		// catch >=400 status codes so a 404 error page does not
		// silently flow into json.Unmarshal as included content.
		var includeData []byte
		if strings.HasPrefix(rn.Include, "http://") || strings.HasPrefix(rn.Include, "https://") {
			rsp, err := http.Get(rn.Include)
			if err != nil {
				return fmt.Errorf("fetch include URL %q: %w", rn.Include, err)
			}
			defer rsp.Body.Close()
			if rsp.StatusCode >= 400 {
				return fmt.Errorf("fetch include URL %q: HTTP %d", rn.Include, rsp.StatusCode)
			}
			includeData, err = io.ReadAll(json5.NewTrimNodeReader(rsp.Body))
			if err != nil {
				return fmt.Errorf("read include URL %q body: %w", rn.Include, err)
			}
		} else {
			f, err := os.Open(rn.Include)
			if err != nil {
				return fmt.Errorf("open include file %q: %w", rn.Include, err)
			}
			defer f.Close()
			includeData, err = io.ReadAll(json5.NewTrimNodeReader(f))
			if err != nil {
				return fmt.Errorf("read include file %q: %w", rn.Include, err)
			}
		}
		data = includeData
		if err := json.Unmarshal(data, &rn); err != nil {
			return fmt.Errorf("unmarshal include content from %q: %w", rn.Include, err)
		}
	}

	n.ApiConfig = ApiConfig{
		APIHost: "http://127.0.0.1",
		Timeout: 30,
	}
	if len(rn.ApiRaw) > 0 {
		err = json.Unmarshal(rn.ApiRaw, &n.ApiConfig)
		if err != nil {
			return
		}
	} else {
		err = json.Unmarshal(data, &n.ApiConfig)
		if err != nil {
			return
		}
	}

	n.Options = Options{
		ListenIP:   "0.0.0.0",
		SendIP:     "0.0.0.0",
		CertConfig: NewCertConfig(),
	}
	if len(rn.OptRaw) > 0 {
		err = json.Unmarshal(rn.OptRaw, &n.Options)
		if err != nil {
			return
		}
	} else {
		err = json.Unmarshal(data, &n.Options)
		if err != nil {
			return
		}
	}
	return
}

type Options struct {
	Name                   string          `json:"Name"`
	Core                   string          `json:"Core"`
	CoreName               string          `json:"CoreName"`
	ListenIP               string          `json:"ListenIP"`
	SendIP                 string          `json:"SendIP"`
	DeviceOnlineMinTraffic int64           `json:"DeviceOnlineMinTraffic"`
	LimitConfig            LimitConfig     `json:"LimitConfig"`
	RawOptions             json.RawMessage `json:"RawOptions"`
	XrayOptions            *XrayOptions    `json:"XrayOptions"`
	SingOptions            *SingOptions    `json:"SingOptions"`
	CertConfig             *CertConfig     `json:"CertConfig"`
}

func (o *Options) UnmarshalJSON(data []byte) error {
	type opt Options
	err := json.Unmarshal(data, (*opt)(o))
	if err != nil {
		return err
	}
	switch o.Core {
	case "xray":
		o.XrayOptions = NewXrayOptions()
		return json.Unmarshal(data, o.XrayOptions)
	case "sing":
		o.SingOptions = NewSingOptions()
		return json.Unmarshal(data, o.SingOptions)
	default:
		o.Core = ""
		o.RawOptions = data
	}
	return nil
}
